// Command kwerft runs the Kwerft console: REST/WebSocket API, the reconcilers
// for kwerft.dev resources, and the embedded web UI — one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/server"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/version"
	"github.com/ehilzinger/kwerft/web"
)

func main() {
	var (
		listen         = flag.String("listen", ":8080", "HTTP listen address")
		dataDir        = flag.String("data-dir", "/var/lib/kwerft", "directory for the SQLite store")
		consoleDomain  = flag.String("console-domain", "", "public hostname of the console")
		platform       = flag.String("platform", "dedicated", "hosting platform: cloud or dedicated")
		runControllers = flag.Bool("controllers", true, "run the reconcilers (needs cluster access)")
		leaderElect    = flag.Bool("leader-elect", true, "use leader election so only one replica reconciles")
		gatewayClass   = flag.String("gateway-class", "traefik", "GatewayClass of the shared Gateway")
		clusterIssuer  = flag.String("cluster-issuer", "letsencrypt", "cert-manager ClusterIssuer for HTTPS listeners; empty disables certificates")
		dev            = flag.Bool("dev", false, "local development: plain-HTTP cookies and a setup token printed to the log")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if *platform != "cloud" && *platform != "dedicated" {
		log.Error("invalid --platform", "value", *platform)
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("cannot create data directory", "dir", *dataDir, "err", err)
		os.Exit(1)
	}
	key, err := dataKey(log, *dev, *dataDir)
	if err != nil {
		log.Error("cannot start without a data key", "err", err)
		os.Exit(1)
	}
	st, err := store.Open(ctx, filepath.Join(*dataDir, "kwerft.db"))
	if err != nil {
		log.Error("cannot open the database", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	go cleanSessions(ctx, log, st)

	var tokens setup.TokenSource = setup.NewStaticTokenSource("", 0) // expired: no setup possible
	var ready atomic.Bool
	var mgr ctrl.Manager
	if *runControllers {
		mgr, err = newManager(log, *leaderElect, &controllers.DomainReconciler{
			ConsoleDomain: *consoleDomain,
			GatewayClass:  *gatewayClass,
			ClusterIssuer: *clusterIssuer,
		})
		if err != nil {
			log.Error("cannot start controllers", "err", err)
			os.Exit(1)
		}
		namespace := os.Getenv("POD_NAMESPACE")
		if namespace == "" {
			namespace = "kwerft-system"
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
		PasskeyOrigins:  passkeyOrigins,

		Kube:      kubeImp,
		KubeCache: kubeCache,
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

// cleanSessions deletes expired sessions once an hour.
func cleanSessions(ctx context.Context, log *slog.Logger, st *store.Store) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n, err := st.DeleteExpiredSessions(ctx, time.Now()); err != nil {
			log.Error("session cleanup failed", "err", err)
		} else if n > 0 {
			log.Info("expired sessions removed", "count", n)
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
		&kwerftv1.Volume{}, &kwerftv1.Task{}, &kwerftv1.Schedule{}}
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

func newManager(log *slog.Logger, leaderElect bool, domains *controllers.DomainReconciler) (ctrl.Manager, error) {
	ctrl.SetLogger(logr.FromSlogHandler(log.Handler()))
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  controllers.NewScheme(),
		Metrics:                 metricsserver.Options{BindAddress: "0"}, // TODO(phase-3): expose for vmagent
		HealthProbeBindAddress:  "0",                                     // the console server answers /healthz and /readyz
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
	if err := (&controllers.ProjectReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if err := (&controllers.AppReconciler{Client: mgr.GetClient()}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	domains.Client = mgr.GetClient()
	if err := domains.SetupWithManager(mgr); err != nil {
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
	return mgr, nil
}
