// Command werft runs the Werft console: REST/WebSocket API, the reconcilers
// for werft.dev resources, and the embedded web UI — one binary.
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	werftv1 "github.com/ehilzinger/werft/api/v1alpha1"
	"github.com/ehilzinger/werft/internal/controllers"
	"github.com/ehilzinger/werft/internal/server"
	"github.com/ehilzinger/werft/internal/version"
	"github.com/ehilzinger/werft/web"
)

func main() {
	var (
		listen         = flag.String("listen", ":8080", "HTTP listen address")
		dataDir        = flag.String("data-dir", "/var/lib/werft", "directory for the SQLite store")
		consoleDomain  = flag.String("console-domain", "", "public hostname of the console")
		platform       = flag.String("platform", "dedicated", "hosting platform: cloud or dedicated")
		runControllers = flag.Bool("controllers", true, "run the reconcilers (needs cluster access)")
		leaderElect    = flag.Bool("leader-elect", true, "use leader election so only one replica reconciles")
		gatewayClass   = flag.String("gateway-class", "traefik", "GatewayClass of the shared Gateway")
		clusterIssuer  = flag.String("cluster-issuer", "letsencrypt", "cert-manager ClusterIssuer for HTTPS listeners; empty disables certificates")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if *platform != "cloud" && *platform != "dedicated" {
		log.Error("invalid --platform", "value", *platform)
		os.Exit(2)
	}

	// TODO(phase-1): open the SQLite store in dataDir.
	_ = dataDir

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var ready atomic.Bool
	if *runControllers {
		mgr, err := newManager(log, *leaderElect, &controllers.DomainReconciler{
			ConsoleDomain: *consoleDomain,
			GatewayClass:  *gatewayClass,
			ClusterIssuer: *clusterIssuer,
		})
		if err != nil {
			log.Error("cannot start controllers", "err", err)
			os.Exit(1)
		}
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

	srv := server.New(server.Config{
		Listen:        *listen,
		ConsoleDomain: *consoleDomain,
		Platform:      *platform,
		UI:            web.Assets(),
		Logger:        log,
		Ready:         ready.Load,
	})

	go func() {
		log.Info("werft starting", "version", version.Version, "commit", version.Commit,
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
	log.Info("werft stopped")
}

// waitUntilReady marks the server ready once the caches for Werft's own types
// have synced. Registering them up front matters: controllers only start their
// informers after winning leader election, and a missing CRD must keep the pod
// not-ready (so `helm --wait` fails) instead of passing silently.
func waitUntilReady(ctx context.Context, log *slog.Logger, mgr ctrl.Manager, ready *atomic.Bool) {
	types := []client.Object{&werftv1.Project{}, &werftv1.App{}, &werftv1.Domain{}}
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
		LeaderElectionID:        "werft-controllers",
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
	return mgr, nil
}
