// Command kwerft runs the Kwerft console: REST/WebSocket API, the reconcilers
// for kwerft.dev resources, and the embedded web UI — one binary. `kwerft
// agent` runs the reconcilers in a remote cluster with a tunnel to the
// console instead (agent.go); `kwerft node-agent` is the firewall and etcd
// snapshot DaemonSets.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/hubble"
	"github.com/ehilzinger/kwerft/internal/metrics"
	"github.com/ehilzinger/kwerft/internal/observability"
	"github.com/ehilzinger/kwerft/internal/server"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/upgrades"
	"github.com/ehilzinger/kwerft/internal/version"
	"github.com/ehilzinger/kwerft/web"
)

func main() {
	// `kwerft node-agent` is the firewall DaemonSet (nodeagent.go).
	if len(os.Args) > 1 && os.Args[1] == "node-agent" {
		os.Exit(runNodeAgent(os.Args[2:]))
	}
	// `kwerft db-snapshot` is Velero's pre-backup hook on the console pod (dbsnapshot.go).
	if len(os.Args) > 1 && os.Args[1] == "db-snapshot" {
		os.Exit(runDBSnapshot(os.Args[2:]))
	}
	// `kwerft etcd-snapshot list|fetch` reads uploaded etcd snapshots back
	// from the backup bucket, decrypted (etcdsnapshot.go).
	if len(os.Args) > 1 && os.Args[1] == "etcd-snapshot" {
		os.Exit(runEtcdSnapshot(os.Args[2:]))
	}
	// `kwerft upgrade-runner` is an Upgrade's runner pod (upgraderunner.go).
	if len(os.Args) > 1 && os.Args[1] == "upgrade-runner" {
		os.Exit(runUpgradeRunner(os.Args[2:]))
	}
	// `kwerft agent` takes the console's flags for the reconcilers, plus its own.
	agentMode := len(os.Args) > 1 && os.Args[1] == "agent"
	if agentMode {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
	var (
		listen         = flag.String("listen", ":8080", "HTTP listen address")
		dataDir        = flag.String("data-dir", "/var/lib/kwerft", "directory for the SQLite store")
		consoleDomain  = flag.String("console-domain", "", "public hostname of the console")
		platform       = flag.String("platform", "dedicated", "hosting platform: cloud or dedicated")
		runControllers = flag.Bool("controllers", true, "run the reconcilers (needs cluster access)")
		metricsListen  = flag.String("metrics-listen", ":8081", "address of Kwerft's own Prometheus metrics (with the reconcilers only; scraped by vmagent inside the cluster, never routed through the Gateway); \"0\" disables")
		leaderElect    = flag.Bool("leader-elect", true, "use leader election so only one replica reconciles")
		gatewayClass   = flag.String("gateway-class", "traefik", "GatewayClass of the shared Gateway")
		clusterIssuer  = flag.String("cluster-issuer", "letsencrypt", "cert-manager ClusterIssuer for HTTPS listeners; empty disables certificates")
		debugImage     = flag.String("debug-image", server.DefaultDebugImage, "toolbox image for debug shells into containers without a shell")
		dev            = flag.Bool("dev", false, "local development: plain-HTTP cookies and a setup token printed to the log")
		privateNetwork = flag.String("private-network", "", "the cluster's private network (Cloud Network or vSwitch) as a CIDR, from install.sh; shown in the required firewall rules")

		buildkitImage       = flag.String("buildkit-image", controllers.DefaultBuildKitImage, "rootless BuildKit image for builds (also clones the repository)")
		railpackImage       = flag.String("railpack-image", controllers.DefaultRailpackImage, "Railpack frontend image (railpack prepare and the BuildKit frontend)")
		maxConcurrentBuilds = flag.Int("max-concurrent-builds", 1, "builds running at once in the cluster; more wait in a queue")
		buildTimeout        = flag.Duration("build-timeout", controllers.DefaultBuildTimeout, "a build running longer fails")
		buildAppArmor       = flag.String("build-apparmor-profile", "kwerft-buildkit", "AppArmor profile (loaded on every node) build containers run under; empty runs them unconfined")
		hubbleRelay         = flag.String("hubble-relay", hubble.DefaultRelayAddress, "Hubble relay (host:port, plain gRPC) for traffic rule counts and dropped connections; empty turns them off (install.sh --lite has no Hubble)")

		consoleURL         = flag.String("console-url", os.Getenv("KWERFT_CONSOLE_URL"), "agent mode: the console to connect to, https://<console>")
		agentTokenFile     = flag.String("agent-token-file", "/etc/kwerft-agent/token", "agent mode: file with this cluster's agent token (the mounted Secret kwerft-agent)")
		hcloudCCM          = flag.Bool("hcloud-ccm", false, "the hcloud cloud-controller-manager runs in the cluster (install.sh, chosen at the first install)")
		hcloudProxyNetwork = flag.String("hcloud-proxy-network", "", "private network (CIDR) the ingress accepts the PROXY protocol from, for a Hetzner Load Balancer in front of it (install.sh); empty: none")

		installBaseURL = flag.String("install-base-url", "", "the install repository's raw files, for release discovery and upgrades (empty: "+upgrades.DefaultInstallBaseURL+")")
		upgradeFaults  = flag.Bool("upgrade-faults", false, "e2e only: pass an Upgrade's kwerft.dev/e2e-fault annotation on to its runner (chart value e2e.faults)")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if *platform != "cloud" && *platform != "dedicated" {
		log.Error("invalid --platform", "value", *platform)
		os.Exit(2)
	}
	if *privateNetwork != "" {
		if p, err := netip.ParsePrefix(*privateNetwork); err != nil || p.Masked() != p {
			log.Error("invalid --private-network: want a network like 10.0.0.0/16", "value", *privateNetwork)
			os.Exit(2)
		}
	}
	if *hcloudProxyNetwork != "" {
		if p, err := netip.ParsePrefix(*hcloudProxyNetwork); err != nil || p.Masked() != p {
			log.Error("invalid --hcloud-proxy-network: want a network like 10.0.0.0/16", "value", *hcloudProxyNetwork)
			os.Exit(2)
		}
	}
	if *maxConcurrentBuilds < 1 || *buildTimeout <= 0 {
		log.Error("invalid build settings", "max-concurrent-builds", *maxConcurrentBuilds, "build-timeout", *buildTimeout)
		os.Exit(2)
	}

	namespace := os.Getenv("POD_NAMESPACE")
	if namespace == "" {
		namespace = "kwerft-system"
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// The reconcilers, the same in the console's cluster and (agent mode)
	// in remote ones.
	var flows *hubble.Aggregator
	// The console's store, for the upgrade's database copy (set once it is
	// open; agent mode has none).
	var database upgrades.Snapshotter
	// The upgrade preflight, for the console API (Settings › Updates).
	var upgradeChecks *controllers.UpgradeChecks
	newControllers := func() (ctrl.Manager, error) {
		traffic := &controllers.TrafficRuleReconciler{}
		// Cilium's flows, read in-cluster from the Hubble relay: the
		// console's traffic views and the TrafficRules' status counts.
		if *hubbleRelay != "" {
			flows = hubble.NewAggregator()
			flows.Logger = log
			go flows.Run(ctx, hubble.NewRelay(*hubbleRelay))
			traffic.Counts = flows
		}
		mgr, err := newManager(log, *leaderElect, *metricsListen, *privateNetwork, !agentMode, traffic, &controllers.DomainReconciler{
			ConsoleDomain: *consoleDomain,
			GatewayClass:  *gatewayClass,
			ClusterIssuer: *clusterIssuer,
		}, &controllers.BuildReconciler{
			BuildKitImage:       *buildkitImage,
			RailpackImage:       *railpackImage,
			MaxConcurrentBuilds: *maxConcurrentBuilds,
			Timeout:             *buildTimeout,
			AppArmorProfile:     *buildAppArmor,
		})
		if err != nil {
			return nil, err
		}
		// This cluster's name in Cloud labels: local for the console's,
		// the agent token's cluster otherwise.
		name := clusters.Local
		if agentMode {
			token, err := readToken(*agentTokenFile)()
			if err != nil {
				return nil, err
			}
			name, _ = clusters.AgentTokenCluster(token)
		}
		if upgradeChecks, err = setupUpgrades(mgr, upgradeOptions{namespace: namespace, installBaseURL: *installBaseURL, faults: *upgradeFaults,
			console: !agentMode, cluster: name, dataDir: *dataDir, database: database}); err != nil {
			return nil, err
		}
		return mgr, setupEveryCluster(mgr, name, *hcloudProxyNetwork)
	}
	if agentMode {
		os.Exit(runAgent(ctx, log, agentOptions{consoleURL: *consoleURL, tokenFile: *agentTokenFile, namespace: namespace, listen: *listen}, newControllers))
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("cannot create data directory", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	key, err := dataKey(log, *dev, *dataDir)
	if err != nil {
		log.Error("cannot start without a data key", "err", err)
		os.Exit(1)
	}
	previousKeys, err := dataKeyPrevious()
	if err != nil {
		log.Error("invalid previous data key", "err", err)
		os.Exit(1)
	}
	// After install.sh --restore: the backup's consistent copy replaces the
	// restored live files before anything opens them (dbsnapshot.go).
	if _, err := applyRestoredDatabase(*dataDir, log); err != nil {
		log.Error("cannot restore the database from the backup", "err", err)
		os.Exit(1)
	}
	dbFile := filepath.Join(*dataDir, "kwerft.db")
	if *runControllers {
		// A rolled-back upgrade may ask this version to restore its copy.
		restoreDatabase(ctx, log, *dataDir, dbFile)
	}
	st, err := store.Open(ctx, dbFile)
	if err != nil {
		log.Error("cannot open the database", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	database = st
	go cleanSessions(ctx, log, st)

	var tokens setup.TokenSource = setup.NewStaticTokenSource("", 0) // expired: no setup possible
	var ready atomic.Bool
	var mgr ctrl.Manager
	// The clusters this console manages: this one, and remote ones through
	// their agents' tunnels (internal/clusters). Only with the reconcilers,
	// which keep the Cluster objects.
	var registry clusters.Registry
	var hub *clusters.Hub
	if *runControllers {
		mgr, err = newControllers()
		if err != nil {
			log.Error("cannot start controllers", "err", err)
			os.Exit(1)
		}
		hub = &clusters.Hub{Local: mgr.GetConfig(), Clusters: mgr.GetCache(), Logger: log.With("component", "tunnel")}
		registry = hub
		if err := setupClusters(mgr, hub, namespace, *consoleDomain); err != nil {
			log.Error("cannot start the cluster reconciler", "err", err)
			os.Exit(1)
		}
		// Node pools (Cloud servers as nodes) of every cluster, reached
		// through the hub.
		if err := setupNodes(mgr, namespace, key, hub, consoleURLFunc(*consoleDomain, activeConsoleDomain(mgr, *dev))); err != nil {
			log.Error("cannot start the node pool reconcilers", "err", err)
			os.Exit(1)
		}
		// Agent clusters' upgrades of an "Upgrade all", one after another.
		if err := setupAgentUpgrades(mgr, hub); err != nil {
			log.Error("cannot start the agent upgrade reconciler", "err", err)
			os.Exit(1)
		}
		tokens = &setup.SecretTokenSource{Reader: mgr.GetAPIReader(), Writer: mgr.GetClient(), Namespace: namespace}
		go waitUntilReady(ctx, log, mgr, &ready)
		go func() {
			if err := mgr.Start(ctx); err != nil {
				log.Error("controller manager stopped", "err", err)
				os.Exit(1)
			}
		}()
	} else {
		ready.Store(true)
		if cfg, err := ctrl.GetConfig(); err == nil {
			registry = &clusters.Static{Config: cfg}
		}
	}
	if *dev {
		token := "kwft_setup_" + auth.NewToken()[:24]
		tokens = setup.NewStaticTokenSource(token, 24*time.Hour)
		log.Warn("development mode: plain-HTTP cookies; setup token for /setup", "token", token)
	}

	var passkeyOrigins []string
	if *dev {
		if *consoleDomain == "" {
			*consoleDomain = "localhost"
		}
		passkeyOrigins = devPasskeyOrigins(*listen)
	}
	kubeImp, kubeCache := workloadAccess(log, mgr)
	// The console's own identity, for webhook builds and Git credentials
	// (see internal/server/api_git.go); only with the controller manager.
	var system client.Client
	var systemReader client.Reader
	var metricsClient *metrics.Client
	var trustedProxy func(netip.Addr) bool
	var dataKeySecret types.NamespacedName
	if mgr != nil {
		system, systemReader = mgr.GetClient(), mgr.GetAPIReader()
		metricsClient = metrics.New(observability.MetricsURL) // in-cluster only
		// X-Real-Ip only from Traefik, i.e. from a node's address
		// (internal/server/clientip.go).
		peers := server.NewNodePeers(systemReader, log)
		go peers.Run(ctx)
		trustedProxy = peers.Trusted
		dataKeySecret = types.NamespacedName{Namespace: namespace, Name: cmp.Or(os.Getenv("KWERFT_DATA_KEY_SECRET"), "kwerft-data-key")}
	}

	var preflight server.UpgradePreflight
	if upgradeChecks != nil {
		preflight = &upgradePreflight{local: upgradeChecks}
	}

	srv := server.New(server.Config{
		Listen:        *listen,
		ConsoleDomain: *consoleDomain,
		Platform:      *platform,
		UI:            web.Assets(),
		Logger:        log,
		Ready:         ready.Load,

		Store:           st,
		SetupTokens:     tokens,
		InsecureCookies: *dev,
		DataKey:         key,
		DataKeyPrevious: previousKeys,
		DataKeySecret:   dataKeySecret,
		TrustedProxy:    trustedProxy,
		PasskeyOrigins:  passkeyOrigins,

		Kube:          kubeImp,
		KubeCache:     kubeCache,
		Clusters:      registry,
		BaseContext:   ctx,
		RecordingsDir: filepath.Join(*dataDir, "recordings"),
		DebugImage:    *debugImage,

		System:       system,
		SystemReader: systemReader,
		Git:          gitFactory,
		Metrics:      metricsClient,
		Hubble:       flows,

		Tunnel: hub,

		ActiveConsoleDomain: activeConsoleDomain(mgr, *dev),

		HCloudCCM:          *hcloudCCM,
		HCloudProxyNetwork: *hcloudProxyNetwork,
		StorageClassExists: storageClassChecker(mgr),
		Upgrades:           preflight,
	})

	go func() {
		log.Info("kwerft starting", "version", version.Version, "commit", version.Commit,
			"listen", *listen, "platform", *platform, "controllers", *runControllers)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("graceful shutdown failed", "err", err)
	}
	log.Info("kwerft stopped")
}

// cleanSessions deletes expired sessions once an hour, and API tokens a
// while after they expired.
func cleanSessions(ctx context.Context, log *slog.Logger, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(ctx, time.Now()); err != nil {
			log.Error("session cleanup failed", "err", err)
		} else if n > 0 {
			log.Info("expired sessions removed", "count", n)
		}
		if n, err := st.DeleteExpiredTokens(ctx, time.Now(), server.TokenExpiredKeep); err != nil {
			log.Error("API token cleanup failed", "err", err)
		} else if n > 0 {
			log.Info("expired API tokens removed", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// waitUntilReady marks the server ready once the caches for Kwerft's own types
// have synced. Registering them up front matters: controllers only start their
// informers after winning leader election, and a missing CRD must keep the pod
// not-ready (so `helm --wait` fails) instead of passing silently.
func waitUntilReady(ctx context.Context, log *slog.Logger, mgr ctrl.Manager, ready *atomic.Bool) {
	types := []client.Object{&kwerftv1.Project{}, &kwerftv1.App{}, &kwerftv1.Domain{},
		&kwerftv1.Volume{}, &kwerftv1.Task{}, &kwerftv1.Schedule{}, &kwerftv1.ConsoleSettings{},
		&kwerftv1.GitConnection{}, &kwerftv1.Build{}, &kwerftv1.AlertRule{}, &kwerftv1.NotificationChannel{},
		&kwerftv1.FirewallRule{}, &kwerftv1.Cluster{}, &kwerftv1.NodePool{}, &kwerftv1.BackupPlan{}, &kwerftv1.Restore{}, &kwerftv1.SecretSet{}}
	for _, obj := range types {
		for {
			_, err := mgr.GetCache().GetInformer(ctx, obj, cache.BlockUntilSynced(false))
			if err == nil {
				break
			}
			log.Error("waiting for CRD", "type", fmt.Sprintf("%T", obj), "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}
	if mgr.GetCache().WaitForCacheSync(ctx) {
		ready.Store(true)
	}
}

// console: the console's cluster (not agent mode), whose DNS reconciler
// also keeps the records of remote clusters' hostnames.
func newManager(log *slog.Logger, leaderElect bool, metricsListen, privateNetwork string, console bool, traffic *controllers.TrafficRuleReconciler, domains *controllers.DomainReconciler, builds *controllers.BuildReconciler) (ctrl.Manager, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: controllers.NewScheme(),
		// Kwerft's own metrics and controller-runtime's (internal/controllers/metrics.go),
		// on a port of their own: the console port is what the Gateway routes to.
		Metrics:                 metricsserver.Options{BindAddress: metricsListen},
		HealthProbeBindAddress:  "0", // the console server answers /healthz and /readyz
		LeaderElection:          leaderElect,
		LeaderElectionID:        "kwerft-controllers",
		LeaderElectionNamespace: os.Getenv("POD_NAMESPACE"),
		// Hand the lease over on shutdown so an upgraded pod takes over at once
		// instead of waiting for the old lease to expire.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return nil, err
	}
	if err := controllers.RegisterMetrics(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.ProjectReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	apps := &controllers.AppReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Registry: &controllers.RegistryKeeper{URL: controllers.DefaultRegistryURL}}
	if err := apps.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	// TrafficRules → CiliumNetworkPolicies, with Hubble's counts (Phase 4).
	traffic.Client = mgr.GetClient()
	if err := traffic.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	domains.Client = mgr.GetClient()
	domains.APIReader = mgr.GetAPIReader()
	if err := domains.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	dns := &controllers.DNSReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ConsoleDomain: domains.ConsoleDomain,
		RemoteClusters: console}
	if err := dns.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.VolumeReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.TaskReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.ScheduleReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	// Secret sets (Phase 6): their Secrets, generated and derived keys, and
	// the Roles that make values write-only.
	if err := (&controllers.SecretSetReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	gitConnections := &controllers.GitConnectionReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Git: gitFactory, ConsoleDomain: domains.ConsoleDomain}
	if err := gitConnections.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	commitStatus := &controllers.CommitStatusReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(),
		Git: gitFactory, ConsoleDomain: domains.ConsoleDomain}
	if err := commitStatus.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	builds.Client = mgr.GetClient()
	builds.APIReader = mgr.GetAPIReader()
	if err := builds.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	// Alerting: AlertRules → the VMRule, channels → Alertmanager (Phase 3).
	if err := (&controllers.AlertRuleReconciler{Client: mgr.GetClient(), ConsoleDomain: domains.ConsoleDomain}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.NotificationChannelReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	// Server firewall (Phase 4): FirewallRules → the node agents' desired rules.
	if err := (&controllers.FirewallReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), PrivateNetwork: privateNetwork}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	// Backups (Phase 6): the target, BackupPlans and Restores → Velero.
	if err := (&controllers.BackupTargetReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), ConsoleDomain: domains.ConsoleDomain}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.BackupPlanReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.RestoreReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	return mgr, nil
}

// storageClassChecker reads StorageClasses through the manager's cache;
// nil without the controllers.
func storageClassChecker(mgr ctrl.Manager) func(context.Context, string) bool {
	if mgr == nil {
		return nil
	}
	return server.StorageClassChecker(mgr.GetClient())
}

// gitFactory is shared by the Git reconcilers and the console, so GitHub App
// installation tokens are cached once.
var gitFactory = &git.Factory{}
