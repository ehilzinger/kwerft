// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/go-logr/logr"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// k8s is a client for the test API server; nil when envtest is unavailable.
var k8s client.Client

const testConsoleDomain = "console.example.com"

// testSecretsHashKey is the App reconciler's secrets hash key.
var testSecretsHashKey = []byte("secrets hash key of the tests!!!")

// TestMain starts a real kube-apiserver + etcd (envtest) with the Kwerft and
// Gateway API CRDs, and runs both reconcilers against it. There are no
// built-in controllers (no pods, no garbage collection), so tests assert on
// rendered objects and fake workload status where needed.
//
// Needs KUBEBUILDER_ASSETS (set by `make test`); without it these tests skip.
func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Println("KUBEBUILDER_ASSETS not set — skipping controller integration tests (run `make test`)")
		os.Exit(m.Run())
	}
	log.SetLogger(logr.Discard())

	gatewayCRDs, err := moduleDir("sigs.k8s.io/gateway-api")
	if err != nil {
		fmt.Println("locate gateway-api module:", err)
		os.Exit(1)
	}
	env := &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("..", "..", "charts", "kwerft", "crds"),
			filepath.Join(gatewayCRDs, "config", "crd", "standard"),
			filepath.Join("testdata", "crds"),
		},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Println("start envtest:", err)
		os.Exit(1)
	}

	k8s, err = client.New(cfg, client.Options{Scheme: NewScheme()})
	if err != nil {
		fmt.Println("client:", err)
		os.Exit(1)
	}
	// The shared Gateway lives here; the installer creates it in real clusters.
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: GatewayNamespace}}); err != nil {
		fmt.Println("create gateway namespace:", err)
		os.Exit(1)
	}
	// Builds run in kwerft-builds and push to the registry Service; the
	// chart creates both in real clusters.
	if err := setupBuildInfra(context.Background()); err != nil {
		fmt.Println("build infrastructure:", err)
		os.Exit(1)
	}
	// Backups: Velero's namespace (install.sh in real clusters).
	if err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: VeleroNamespace}}); err != nil {
		fmt.Println("create velero namespace:", err)
		os.Exit(1)
	}
	// Alerting: the observability namespace and the stack's VMAlertmanager
	// (install.sh in real clusters).
	if err := setupObservability(context.Background()); err != nil {
		fmt.Println("observability:", err)
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 NewScheme(),
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		fmt.Println("manager:", err)
		os.Exit(1)
	}
	must((&ProjectReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	must((&AppReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), SecretsHashKey: testSecretsHashKey}).SetupWithManager(mgr))
	must((&TrafficRuleReconciler{Client: mgr.GetClient(), Counts: counts}).SetupWithManager(mgr))
	must((&DomainReconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		Now:           domainClock.Now,
		ConsoleDomain: testConsoleDomain,
		GatewayClass:  "traefik",
		ClusterIssuer: "letsencrypt",
	}).SetupWithManager(mgr))
	must((&VolumeReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	must((&TaskReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Now: testClock.Now}).SetupWithManager(mgr))
	must((&ScheduleReconciler{Client: mgr.GetClient(), Now: testClock.Now}).SetupWithManager(mgr))
	must((&SecretSetReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr))
	must((&BuildReconciler{
		Client:              mgr.GetClient(),
		APIReader:           mgr.GetAPIReader(),
		BuildKitImage:       DefaultBuildKitImage,
		RailpackImage:       DefaultRailpackImage,
		MaxConcurrentBuilds: 1,
		Timeout:             DefaultBuildTimeout,
		InstallationToken:   fakeInstallationToken,
		// The commit status tests set Build phases themselves.
		Ignore: func(b *kwerftv1.Build) bool { return b.Namespace == "gitstatus" },
	}).SetupWithManager(mgr))

	// Registry credentials (registry_auth_test.go): no zot; the probe says
	// what tests want it to.
	must((&RegistryAuthReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Probe: testRegistryProbe}).SetupWithManager(mgr))

	must((&AlertRuleReconciler{Client: mgr.GetClient(), ConsoleDomain: testConsoleDomain}).SetupWithManager(mgr))
	must((&NotificationChannelReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Now: channelClock.Now}).SetupWithManager(mgr))
	must((&FirewallReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), PrivateNetwork: testPrivateNetwork}).SetupWithManager(mgr))
	// Backups (backup_test.go): the tests play Velero.
	must((&BackupTargetReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ConsoleDomain: testConsoleDomain}).SetupWithManager(mgr))
	must((&BackupPlanReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	must((&RestoreReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr))
	// Clusters (cluster_controller_test.go): agents come and go through a fake tunnel.
	must((&ClusterReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Tunnel: testTunnel, Remote: testTunnel.remote,
		Namespace: GatewayNamespace, ConsoleDomain: testConsoleDomain, Resync: 500 * time.Millisecond,
		Local: func(context.Context) clusters.AgentInfo {
			return clusters.AgentInfo{KubernetesVersion: "v-test", Nodes: 1, ReadyNodes: 1}
		}}).SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = mgr.Start(ctx) }()

	code := m.Run()
	cancel()
	_ = env.Stop()
	os.Exit(code)
}

// domainClock is the Domain reconciler's, separate from testClock so moving
// it (the console's redirect grace period) does not make Schedules due.
var domainClock = &offsetClock{}

// channelClock is the NotificationChannel reconciler's (how long a new
// channel may wait for its credentials).
var channelClock = &offsetClock{}

// testClock is the Task and Schedule reconcilers' clock: real time plus an
// offset that tests move forward to make runs due.
var testClock = &offsetClock{}

type offsetClock struct{ offset atomic.Int64 }

func (c *offsetClock) Now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }

// Advance moves the clock forward by d.
func (c *offsetClock) Advance(d time.Duration) { c.offset.Add(int64(d)) }

// Reset returns to real time. Schedule tests start with it: a Schedule's
// creation time is real, so an offset left by an earlier test would make a
// run due at once.
func (c *offsetClock) Reset() { c.offset.Store(0) }

func moduleDir(mod string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", mod).Output()
	return strings.TrimSpace(string(out)), err
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if k8s == nil {
		t.Skip("envtest not available")
	}
}

// eventually retries check until it returns nil or the timeout passes.
func eventually(t *testing.T, check func() error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = check(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("condition not met within 10s: %v", err)
}

// readyReason returns the Ready condition's reason once it reflects the
// object's current generation.
func readyReason(conds []metav1.Condition, generation int64) (string, error) {
	c := meta.FindStatusCondition(conds, ConditionReady)
	if c == nil {
		return "", fmt.Errorf("no Ready condition yet")
	}
	if c.ObservedGeneration != generation {
		return "", fmt.Errorf("Ready condition is for generation %d, want %d", c.ObservedGeneration, generation)
	}
	return c.Reason, nil
}
