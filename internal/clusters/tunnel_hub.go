// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/http2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
)

// TokenHashAnnotation on a Cluster holds the SHA-256 (hex) of its agent's
// token. Owners and admins set it through the console (adopt, rotate), as
// themselves; the Cluster reconciler sets it for clusters Kwerft creates.
// A hash of a 256-bit random token reveals nothing.
const TokenHashAnnotation = "kwerft.dev/agent-token-hash"

// ErrRejected: the agent's token (or request) was refused.
var ErrRejected = errors.New("agent rejected")

// Hub is the console's side of the tunnel and the Registry built on it:
// Local plus every remote cluster whose agent is connected.
type Hub struct {
	// Local is the management cluster: the console's own identity.
	Local *rest.Config
	// Clusters reads Cluster objects in the management cluster (the
	// manager's cache): which clusters exist, and their token hashes.
	// Without it, every agent is refused.
	Clusters client.Reader
	Logger   *slog.Logger
	// InfoInterval is how often agents are asked for their health; zero
	// means 30 seconds.
	InfoInterval time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	mu       sync.Mutex
	sessions map[string]*session
	changed  chan struct{}
}

var _ Registry = (*Hub)(nil)

type session struct {
	cluster   string
	tokenHash string
	remote    string
	conn      *wsConn
	cc        *http2.ClientConn
	ln        net.Listener
	key       string
	since     time.Time

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	info     AgentInfo
	lastSeen time.Time
}

// AgentStatus is what the console knows about a connected agent.
type AgentStatus struct {
	Info     AgentInfo // without ProxyKey
	Since    time.Time // connected since
	LastSeen time.Time // last answer
	Remote   string    // the agent's address as the console saw it
}

func (h *Hub) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Hub) log() *slog.Logger {
	if h.Logger != nil {
		return h.Logger
	}
	return slog.Default()
}

// ---- Registry --------------------------------------------------------------

// List returns Local first, then every Cluster object, by name.
func (h *Hub) List(ctx context.Context) ([]Info, error) {
	out := []Info{{Name: Local, Connected: true}}
	if h.Clusters == nil {
		return out, nil
	}
	var list kwerftv1.ClusterList
	if err := h.Clusters.List(ctx, &list); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var rest []Info
	for _, c := range list.Items {
		if c.Name == Local || c.Spec.Provider == kwerftv1.ClusterLocal {
			continue
		}
		_, ok := h.sessions[c.Name]
		rest = append(rest, Info{Name: c.Name, Connected: ok})
	}
	slices.SortFunc(rest, func(a, b Info) int { return strings.Compare(a.Name, b.Name) })
	return append(out, rest...), nil
}

// RESTConfig reaches a cluster's API server: Local's own config, or for a
// remote cluster the loopback end of its agent's tunnel. Configs of a
// session stop working when it ends; Changed tells when to ask again.
func (h *Hub) RESTConfig(name string) (*rest.Config, error) {
	if name == Local {
		if h.Local == nil {
			return nil, ErrUnavailable
		}
		return rest.CopyConfig(h.Local), nil
	}
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s != nil {
		return &rest.Config{
			Host:        "http://" + s.ln.Addr().String(),
			BearerToken: s.key,
			UserAgent:   "kwerft-console",
			QPS:         50,
			Burst:       100,
		}, nil
	}
	if h.Clusters == nil {
		return nil, ErrUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var c kwerftv1.Cluster
	switch err := h.Clusters.Get(ctx, client.ObjectKey{Name: name}, &c); {
	case apierrors.IsNotFound(err):
		return nil, ErrUnknown
	case err != nil:
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	return nil, ErrUnavailable
}

// Changed is closed and replaced whenever an agent connects or goes away.
func (h *Hub) Changed() <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.changed == nil {
		h.changed = make(chan struct{})
	}
	return h.changed
}

// notify must be called with h.mu held.
func (h *Hub) notify() {
	if h.changed != nil {
		close(h.changed)
	}
	h.changed = make(chan struct{})
}

// Agent reports a connected agent's status.
func (h *Hub) Agent(name string) (AgentStatus, bool) {
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s == nil {
		return AgentStatus{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info := s.info
	info.ProxyKey = ""
	return AgentStatus{Info: info, Since: s.since, LastSeen: s.lastSeen, Remote: s.remote}, true
}

// Disconnect ends the agent session of a cluster, if any (its token was
// rotated or the cluster deleted).
func (h *Hub) Disconnect(name string) {
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s != nil {
		h.log().Info("cluster agent disconnected by the console", "cluster", name)
		s.close()
	}
}

// DisconnectUnless ends a cluster's session unless it authenticated with
// the token whose hash is tokenHash: the token was rotated (or removed)
// since the agent connected.
func (h *Hub) DisconnectUnless(name, tokenHash string) {
	h.mu.Lock()
	s := h.sessions[name]
	h.mu.Unlock()
	if s != nil && (tokenHash == "" || !strings.EqualFold(s.tokenHash, tokenHash)) {
		h.log().Info("cluster agent disconnected: its token is no longer valid", "cluster", name)
		s.close()
	}
}

// ---- the tunnel endpoint ---------------------------------------------------

// Connect serves an agent dialling ConnectPath. It verifies the token,
// upgrades to a WebSocket and serves the session until it ends. It returns
// ErrRejected (having answered 401 or 403) for a refused agent, so the caller
// can count the failure against the client.
func (h *Hub) Connect(w http.ResponseWriter, r *http.Request) error {
	// Agents are programs; a browser page has no business here, and refusing
	// any Origin rules out cross-site WebSocket hijacking outright.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "browsers may not connect here", http.StatusForbidden)
		return ErrRejected
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token = strings.TrimSpace(token)
	name, valid := AgentTokenCluster(token)
	if !ok || !valid {
		http.Error(w, "an agent token is required", http.StatusUnauthorized)
		return ErrRejected
	}
	hash, err := h.verify(r.Context(), name, token)
	if err != nil {
		if errors.Is(err, ErrRejected) {
			h.log().Warn("cluster agent refused", "cluster", name, "reason", err.Error())
			http.Error(w, "the token was not accepted", http.StatusUnauthorized)
			return ErrRejected
		}
		h.log().Error("cluster agent: token check failed", "cluster", name, "err", err)
		http.Error(w, "try again later", http.StatusServiceUnavailable)
		return err
	}
	if !websocket.IsWebSocketUpgrade(r) {
		http.Error(w, "a WebSocket upgrade is required", http.StatusBadRequest)
		return nil
	}
	up := websocket.Upgrader{HandshakeTimeout: 15 * time.Second, CheckOrigin: func(*http.Request) bool { return true }}
	ws, err := up.Upgrade(w, r, nil)
	if err != nil {
		return nil // the upgrader answered
	}
	h.serve(name, hash, remoteAddr(r), newWSConn(ws))
	return nil
}

func remoteAddr(r *http.Request) string {
	if ip := r.Header.Get("X-Real-Ip"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// verify checks token against the Cluster's token hash and returns the hash.
func (h *Hub) verify(ctx context.Context, name, token string) (string, error) {
	if h.Clusters == nil {
		return "", fmt.Errorf("%w: no clusters configured", ErrRejected)
	}
	var c kwerftv1.Cluster
	if err := h.Clusters.Get(ctx, client.ObjectKey{Name: name}, &c); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: no such cluster", ErrRejected)
		}
		return "", err
	}
	if c.DeletionTimestamp != nil || c.Spec.Provider == kwerftv1.ClusterLocal {
		return "", fmt.Errorf("%w: cluster is being deleted or local", ErrRejected)
	}
	hash := c.Annotations[TokenHashAnnotation]
	if hash == "" || !auth.TokenMatches(token, hash) {
		return "", fmt.Errorf("%w: wrong token", ErrRejected)
	}
	return hash, nil
}

// serve runs one agent session until it ends.
func (h *Hub) serve(name, hash, remote string, conn *wsConn) {
	log := h.log().With("cluster", name, "remote", remote)
	t := &http2.Transport{
		AllowHTTP:       true,
		ReadIdleTimeout: 30 * time.Second, // ping an idle agent...
		PingTimeout:     15 * time.Second, // ...and give up on a silent one
		// Queue requests beyond the agent's stream limit instead of
		// failing them: there is no second connection to open.
		StrictMaxConcurrentStreams: true,
		WriteByteTimeout:           30 * time.Second,
	}
	cc, err := t.NewClientConn(conn)
	if err != nil {
		log.Warn("cluster agent: HTTP/2 handshake failed", "err", err)
		_ = conn.Close()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{cluster: name, tokenHash: hash, remote: remote, conn: conn, cc: cc, since: h.now(), ctx: ctx, cancel: cancel}
	defer s.close()

	infoCtx, infoCancel := context.WithTimeout(ctx, 20*time.Second)
	info, err := s.fetchInfo(infoCtx)
	infoCancel()
	if err != nil || info.ProxyKey == "" {
		log.Warn("cluster agent did not answer", "err", err)
		return
	}
	s.key, s.info, s.lastSeen = info.ProxyKey, info, h.now()
	// The loopback end for client-go: see tunnel.go for why not Dial.
	if s.ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		log.Error("cluster agent: no loopback listener", "err", err)
		return
	}
	go s.acceptLoop(log)

	h.mu.Lock()
	if h.sessions == nil {
		h.sessions = map[string]*session{}
	}
	old := h.sessions[name]
	h.sessions[name] = s
	h.notify()
	h.mu.Unlock()
	if old != nil {
		// One connection per cluster: the newest wins, so a half-open old
		// one cannot lock the agent out (a stolen token is fixed by rotating).
		log.Warn("cluster agent replaced an older connection", "previous", old.remote)
		old.close()
	}
	log.Info("cluster agent connected", "agentVersion", info.AgentVersion, "kubernetesVersion", info.KubernetesVersion)

	go h.poll(s, log)
	select {
	case <-conn.Done():
	case <-ctx.Done():
	}

	h.mu.Lock()
	if h.sessions[name] == s {
		delete(h.sessions, name)
		h.notify()
	}
	h.mu.Unlock()
	log.Info("cluster agent disconnected", "connectedFor", h.now().Sub(s.since).Round(time.Second))
}

func (h *Hub) poll(s *session, log *slog.Logger) {
	every := h.InfoInterval
	if every <= 0 {
		every = 30 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	failures := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
		info, err := s.fetchInfo(ctx)
		cancel()
		if err != nil {
			if failures++; failures >= 2 {
				log.Warn("cluster agent stopped answering", "err", err)
				s.close()
				return
			}
			continue
		}
		failures = 0
		s.mu.Lock()
		info.ProxyKey = s.key // fixed for the session
		s.info, s.lastSeen = info, h.now()
		s.mu.Unlock()
	}
}

func (s *session) fetchInfo(ctx context.Context) (AgentInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent"+InfoPath, nil)
	if err != nil {
		return AgentInfo{}, err
	}
	res, err := s.cc.RoundTrip(req)
	if err != nil {
		return AgentInfo{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return AgentInfo{}, fmt.Errorf("agent answered %s", res.Status)
	}
	var info AgentInfo
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&info); err != nil {
		return AgentInfo{}, err
	}
	return info, nil
}

// acceptLoop carries every loopback connection through its own stream.
func (s *session) acceptLoop(log *slog.Logger) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			stream, err := s.openStream()
			if err != nil {
				log.Debug("cluster agent: stream refused", "err", err)
				_ = c.Close()
				return
			}
			pipe(c, stream)
		}()
	}
}

// openStream opens a CONNECT stream to the agent's API proxy.
func (s *session) openStream() (io.ReadWriteCloser, error) {
	pr, pw := io.Pipe()
	req, err := http.NewRequestWithContext(s.ctx, http.MethodConnect, "http://kubernetes", pr)
	if err != nil {
		return nil, err
	}
	req.Host = "kubernetes"
	res, err := s.cc.RoundTrip(req)
	if err != nil {
		_ = pw.Close()
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		_ = res.Body.Close()
		_ = pw.Close()
		return nil, fmt.Errorf("agent answered %s", res.Status)
	}
	return &streamConn{r: res.Body, w: pw, wc: pw}, nil
}

func (s *session) close() {
	s.cancel()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	_ = s.cc.Close()
	_ = s.conn.Close()
}
