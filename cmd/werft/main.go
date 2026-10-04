// Command werft runs the Werft console: REST/WebSocket API, the reconcilers
// for werft.dev resources, and the embedded web UI — one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ehilzinger/werft/internal/server"
	"github.com/ehilzinger/werft/internal/version"
	"github.com/ehilzinger/werft/web"
)

func main() {
	var (
		listen        = flag.String("listen", ":8080", "HTTP listen address")
		dataDir       = flag.String("data-dir", "/var/lib/werft", "directory for the SQLite store")
		consoleDomain = flag.String("console-domain", "", "public hostname of the console")
		platform      = flag.String("platform", "dedicated", "hosting platform: cloud or dedicated")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)

	if *platform != "cloud" && *platform != "dedicated" {
		log.Error("invalid --platform", "value", *platform)
		os.Exit(2)
	}

	// TODO(phase-1): open the SQLite store in dataDir, start the controller
	// manager (App, Project, Build reconcilers) and the informer cache.
	_ = dataDir

	srv := server.New(server.Config{
		Listen:        *listen,
		ConsoleDomain: *consoleDomain,
		Platform:      *platform,
		UI:            web.Assets(),
		Logger:        log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("werft starting", "version", version.Version, "commit", version.Commit, "listen", *listen, "platform", *platform)
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
