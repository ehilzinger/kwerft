// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/version"
)

// agentOptions are `kwerft agent`'s own flags; the reconcilers take the
// console's (main.go).
type agentOptions struct {
	consoleURL string // https://<console>
	tokenFile  string // the agent token, from the mounted Secret
	namespace  string
	listen     string // health probes
}

// checkConsoleURL accepts https://<host>[:port] and nothing else: the
// agent verifies the console's certificate against public CAs.
func checkConsoleURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return fmt.Errorf("--console-url must look like https://ops.example.com, got %q", raw)
	}
	return nil
}

// readToken reads the agent token from the mounted Secret: on every dial,
// so a token the installer rotates is picked up without a restart.
func readToken(path string) func() (string, error) {
	return func() (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		t := strings.TrimSpace(string(b))
		if _, ok := clusters.AgentTokenCluster(t); !ok {
			return "", errors.New("the file does not hold an agent token (kwag_…)")
		}
		return t, nil
	}
}

// runAgent is `kwerft agent`: Kwerft in a remote cluster. The reconcilers run
// as in the console's cluster; instead of the console, a tunnel client dials
// the console and carries its requests to this cluster's API server
// (internal/clusters). It serves only health probes.
func runAgent(ctx context.Context, log *slog.Logger, opts agentOptions, newMgr func() (ctrl.Manager, error)) int {
	if err := checkConsoleURL(opts.consoleURL); err != nil {
		log.Error("invalid agent configuration", "err", err)
		return 2
	}
	if _, err := readToken(opts.tokenFile)(); err != nil {
		log.Error("no agent token", "file", opts.tokenFile, "err", err)
		return 2
	}
	mgr, err := newMgr()
	if err != nil {
		log.Error("cannot start controllers", "err", err)
		return 1
	}
	agent := &clusters.Agent{
		ConsoleURL: strings.TrimSuffix(opts.consoleURL, "/"),
		Token:      readToken(opts.tokenFile),
		API:        mgr.GetConfig(),
		Namespace:  opts.namespace,
		Logger:     log.With("component", "tunnel"),
	}
	// Only the leader dials: two replicas would take turns replacing each
	// other's connection.
	if err := mgr.Add(manager.RunnableFunc(agent.Run)); err != nil {
		log.Error("cannot start the tunnel", "err", err)
		return 1
	}
	var ready atomic.Bool
	go waitUntilReady(ctx, log, mgr, &ready)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "caches not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	srv := &http.Server{Addr: opts.listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server failed", "err", err)
			os.Exit(1)
		}
	}()

	log.Info("kwerft agent starting", "version", version.Version, "commit", version.Commit, "console", opts.consoleURL)
	if err := mgr.Start(ctx); err != nil {
		log.Error("controller manager stopped", "err", err)
		return 1
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	log.Info("kwerft agent stopped")
	return 0
}
