package controllers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
)

// k8s is a client for the test API server; nil when envtest is unavailable.
var k8s client.Client

const testConsoleDomain = "console.example.com"

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
	must((&AppReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr))
	must((&DomainReconciler{
		Client:        mgr.GetClient(),
		ConsoleDomain: testConsoleDomain,
		GatewayClass:  "traefik",
		ClusterIssuer: "letsencrypt",
	}).SetupWithManager(mgr))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = mgr.Start(ctx) }()

	code := m.Run()
	cancel()
	_ = env.Stop()
	os.Exit(code)
}

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
