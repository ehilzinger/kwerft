// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/ehilzinger/kwerft/internal/version"
)

// Agent is the remote cluster's side of the tunnel (`kwerft agent`): it
// dials the console, keeps the connection up, and serves the console's
// requests to its local API server with its own service account.
type Agent struct {
	// ConsoleURL is the console's base URL, https://<console>.
	ConsoleURL string
	// Token returns the agent token. It is read before every dial, so a
	// token rotated in the mounted Secret is picked up without a restart.
	Token func() (string, error)
	// API reaches the local API server with the agent's own identity.
	API *rest.Config
	// Info reports health and join material; nil means LocalInfo with
	// Namespace.
	Info func(ctx context.Context) AgentInfo
	// Namespace holds LocalJoinSecret (kwerft-system).
	Namespace string
	// TLS verifies the console; nil means the system's public CAs.
	TLS    *tls.Config
	Logger *slog.Logger
	// MinBackoff and MaxBackoff bound the wait between dials (1 s, 1 min).
	MinBackoff, MaxBackoff time.Duration
	// Rejected is the wait after the console refused the token (2 min).
	Rejected time.Duration
}

var errDisconnected = errors.New("disconnected")

// Run keeps the agent connected until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	log := a.Logger
	if log == nil {
		log = slog.Default()
	}
	proxy, err := newAPIProxy(a.API)
	if err != nil {
		return err
	}
	info := a.Info
	if info == nil {
		cs, err := kubernetes.NewForConfig(a.API)
		if err != nil {
			return err
		}
		info = func(ctx context.Context) AgentInfo { return LocalInfo(ctx, cs, a.Namespace) }
	}
	b := backoff{min: a.MinBackoff, max: a.MaxBackoff}
	if b.min <= 0 {
		b.min = time.Second
	}
	if b.max <= 0 {
		b.max = time.Minute
	}
	rejectedWait := a.Rejected
	if rejectedWait <= 0 {
		rejectedWait = 2 * time.Minute
	}
	for {
		start := time.Now()
		err := a.session(ctx, proxy, info, log)
		if ctx.Err() != nil {
			return nil
		}
		if time.Since(start) > time.Minute {
			b.reset() // it was up for a while: start over quickly
		}
		wait := b.next()
		if errors.Is(err, ErrRejected) && wait < rejectedWait {
			wait = rejectedWait
		}
		log.Warn("console connection ended; reconnecting", "err", err, "in", wait.Round(100*time.Millisecond))
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
	}
}

func (a *Agent) connectURL() (string, error) {
	u, err := url.Parse(a.ConsoleURL)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http": // tests and local development only
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("console URL %q: want https://<console>", a.ConsoleURL)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + ConnectPath
	return u.String(), nil
}

// session dials the console once and serves until the connection ends.
func (a *Agent) session(ctx context.Context, proxy *apiProxy, info func(context.Context) AgentInfo, log *slog.Logger) error {
	target, err := a.connectURL()
	if err != nil {
		return err
	}
	token, err := a.Token()
	if err != nil {
		return fmt.Errorf("agent token: %w", err)
	}
	dialer := websocket.Dialer{
		TLSClientConfig:  a.TLS,
		HandshakeTimeout: 20 * time.Second,
		Proxy:            http.ProxyFromEnvironment,
	}
	header := http.Header{
		"Authorization": {"Bearer " + strings.TrimSpace(token)},
		"User-Agent":    {"kwerft-agent/" + version.Version},
	}
	ws, res, err := dialer.DialContext(ctx, target, header)
	if err != nil {
		if res != nil && (res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden || res.StatusCode == http.StatusTooManyRequests) {
			return fmt.Errorf("%w: the console answered %s", ErrRejected, res.Status)
		}
		return err
	}
	conn := newWSConn(ws)
	defer conn.Close()
	log.Info("connected to the console", "console", a.ConsoleURL)

	// A fresh key per session: the console's requests to the API proxy
	// carry it (see tunnel.go), nothing else can.
	keyBytes := make([]byte, 32)
	_, _ = rand.Read(keyBytes)
	key := base64.RawURLEncoding.EncodeToString(keyBytes)

	inner := newChanListener()
	innerSrv := &http.Server{Handler: proxy.handler(key), ReadHeaderTimeout: 30 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelDebug)}
	go func() { _ = innerSrv.Serve(inner) }()
	defer innerSrv.Close()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodConnect:
			serveStream(w, r, inner)
		case r.Method == http.MethodGet && r.URL.Path == InfoPath:
			ictx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			i := info(ictx)
			i.ProxyKey = key
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(i)
		default:
			http.NotFound(w, r)
		}
	})
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	srv := &http2.Server{
		MaxConcurrentStreams:         1000,
		ReadIdleTimeout:              30 * time.Second,
		PingTimeout:                  15 * time.Second,
		WriteByteTimeout:             30 * time.Second,
		MaxUploadBufferPerConnection: 16 << 20,
		MaxUploadBufferPerStream:     1 << 20,
	}
	srv.ServeConn(conn, &http2.ServeConnOpts{Context: ctx, Handler: handler})
	return errDisconnected
}

// serveStream turns a CONNECT stream into a connection of the API proxy's
// http.Server. net.Pipe gives that server the deadlines it relies on.
func serveStream(w http.ResponseWriter, r *http.Request, inner *chanListener) {
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	if err := rc.Flush(); err != nil {
		return
	}
	ours, theirs := net.Pipe()
	if !inner.hand(r.Context(), theirs) {
		_ = ours.Close()
		_ = theirs.Close()
		return
	}
	pipe(&streamConn{r: r.Body, w: flushWriter{w, rc}}, ours)
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		err = f.rc.Flush()
	}
	return n, err
}

// apiProxy forwards the console's requests to the local API server, with the
// agent's credentials in place of the session key. Nothing else is reachable
// through it.
type apiProxy struct {
	rp *httputil.ReverseProxy
}

func newAPIProxy(cfg *rest.Config) (*apiProxy, error) {
	if cfg.Impersonate.UserName != "" || len(cfg.Impersonate.Groups) > 0 {
		return nil, fmt.Errorf("the agent's own config must not impersonate")
	}
	target, _, err := rest.DefaultServerUrlFor(cfg)
	if err != nil {
		return nil, err
	}
	rt, err := rest.TransportFor(cfg)
	if err != nil {
		return nil, err
	}
	// Upgrades (SPDY, WebSocket) need HTTP/1.1 end to end; net/http would
	// put a SPDY upgrade on a pooled HTTP/2 connection to the API server.
	h1 := rest.CopyConfig(cfg)
	h1.TLSClientConfig.NextProtos = []string{"http/1.1"}
	upgradeRT, err := rest.TransportFor(h1)
	if err != nil {
		return nil, err
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
		},
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			if httpstream.IsUpgradeRequest(req) {
				return upgradeRT.RoundTrip(req)
			}
			return rt.RoundTrip(req)
		}),
		FlushInterval: -1, // watches and log follows stream as they come
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, "the agent could not reach the API server: "+err.Error(), http.StatusBadGateway)
		},
	}
	return &apiProxy{rp: rp}, nil
}

func (p *apiProxy) handler(key string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(key)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// The agent's own credentials replace the key (client-go adds them
		// only to a request without Authorization). Impersonate-* headers
		// pass: the agent's service account may impersonate exactly the
		// console's users and groups.
		r.Header.Del("Authorization")
		p.rp.ServeHTTP(w, r)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// LocalInfo gathers an agent's report with its own identity: versions, nodes
// and the join material the installer stored in namespace.
func LocalInfo(ctx context.Context, cs kubernetes.Interface, namespace string) AgentInfo {
	info := AgentInfo{AgentVersion: version.Version}
	var problems []string
	if v, err := cs.Discovery().ServerVersion(); err != nil {
		problems = append(problems, "version: "+err.Error())
	} else {
		info.KubernetesVersion = v.GitVersion
	}
	if nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		problems = append(problems, "nodes: "+err.Error())
	} else {
		for _, n := range nodes.Items {
			info.Nodes++
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
					info.ReadyNodes++
				}
			}
		}
	}
	if namespace != "" {
		s, err := cs.CoreV1().Secrets(namespace).Get(ctx, LocalJoinSecret, metav1.GetOptions{})
		switch {
		case err == nil && len(s.Data[SecretJoinServer]) > 0 && len(s.Data[SecretJoinToken]) > 0:
			info.Join = &JoinMaterial{Server: string(s.Data[SecretJoinServer]), Token: string(s.Data[SecretJoinToken])}
		case err != nil && !apierrors.IsNotFound(err):
			problems = append(problems, "join material: "+err.Error())
		}
	}
	info.Error = strings.Join(problems, "; ")
	return info
}
