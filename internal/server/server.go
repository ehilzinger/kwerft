// Package server wires the HTTP API and the embedded console UI.
package server

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/kube"
	"github.com/ehilzinger/kwerft/internal/setup"
	"github.com/ehilzinger/kwerft/internal/store"
	"github.com/ehilzinger/kwerft/internal/version"
)

// Config is the runtime configuration the server needs.
type Config struct {
	Listen        string
	ConsoleDomain string
	Platform      string // "cloud" or "dedicated"
	UI            fs.FS  // built web assets; index.html at the root
	Logger        *slog.Logger
	// Ready reports whether the server can serve real traffic (controller
	// caches synced). Nil means always ready.
	Ready func() bool

	// Store and SetupTokens enable setup, sign-in and the authenticated API.
	// Without a Store only health, version and the UI are served.
	Store       *store.Store
	SetupTokens setup.TokenSource
	// InsecureCookies drops the Secure flag, for http://localhost development.
	InsecureCookies bool
	// Now is the clock; nil means time.Now (tests override it).
	Now func() time.Time
	// DataKey (32 bytes) encrypts secrets at rest, such as TOTP seeds. Without
	// it, authenticator apps cannot be set up or checked.
	DataKey []byte
	// PasskeyOrigins are the browser origins passkeys are accepted from; empty
	// means https://<ConsoleDomain>. The relying party ID is ConsoleDomain, and
	// passkeys are off when that is empty.
	PasskeyOrigins []string

	// Kube enables the workload API (projects, apps); it acts as the
	// signed-in user. KubeCache, optional, serves list endpoints for speed
	// (see api_workloads.go for when that is allowed).
	Kube      *kube.Impersonator
	KubeCache client.Reader
	// RecordingsDir keeps the asciinema recordings of shell sessions; empty
	// disables shells, since none may run unrecorded (see api_shell.go).
	RecordingsDir string

	// podsHook lets tests swap Kubernetes and the limits of the pod
	// endpoints (logs, shells) for fakes; see api_pods.go.
	podsHook func(*podsAPI)
}

// New returns an http.Server ready to ListenAndServe.
func New(cfg Config) *http.Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &http.Server{
		Addr:              cfg.Listen,
		Handler:           Handler(cfg),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Handler builds the route table. Exposed separately for tests.
func Handler(cfg Config) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if cfg.Ready != nil && !cfg.Ready() {
			http.Error(w, "caches not synced", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version":  version.Version,
			"commit":   version.Commit,
			"platform": cfg.Platform,
		})
	})
	if cfg.Store != nil {
		newAPI(cfg).register(mux)
	}
	// Unknown API paths must not fall through to the SPA.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	})

	mux.Handle("/", spa(cfg.UI))

	return logRequests(cfg.Logger, securityHeaders(mux))
}

// spa serves static files and falls back to index.html for client-side routes.
func spa(ui fs.FS) http.Handler {
	files := http.FileServerFS(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		if _, err := fs.Stat(ui, name); errors.Is(err, fs.ErrNotExist) {
			if path.Ext(name) != "" {
				http.NotFound(w, r) // a missing asset, not a route
				return
			}
			serveIndex(w, r, ui)
			return
		}
		if strings.HasPrefix(name, "assets/") {
			// Vite fingerprints everything under assets/.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, ui fs.FS) {
	index, err := fs.ReadFile(ui, "index.html")
	if err != nil {
		http.Error(w, "console UI not built — run `make web`", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(index)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; "+
				"font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush, Hijack and deadlines of
// the real writer (log streams and WebSockets need them).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			return
		}
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
