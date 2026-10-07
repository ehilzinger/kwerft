// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

// The agent tunnel: how the console reaches the Kubernetes API of a remote
// cluster whose API server is never exposed.
//
//	console                                         remote cluster
//	client-go ──► 127.0.0.1:<port> ──┐             ┌──► reverse proxy ──► API server
//	(any request: watch, logs,       │  HTTP/2     │    (the agent's service
//	 SPDY/WebSocket exec)            └─ CONNECT ───┘     account, Impersonate-*
//	                                   streams over       headers passed on)
//	                                   one WebSocket
//	                                   (wss://<console>/api/v1/clusters/connect,
//	                                    dialled by the agent)
//
// The agent (`kwerft agent`) dials out to the console with its cluster's
// token; the console upgrades to a WebSocket and runs an HTTP/2 client over
// it, the agent an HTTP/2 server. HTTP/2 is the multiplexer: per-stream flow
// control, a bounded number of concurrent streams, PING health checks and
// GOAWAY, from golang.org/x/net/http2, already part of the module graph.
//
// Each Kubernetes connection the console opens becomes one HTTP/2 CONNECT
// stream: a byte stream carrying plain HTTP/1.1 to the agent, which serves
// it with an http.Server whose only handler is a reverse proxy to its local
// API server. Because the stream is a byte stream, protocol upgrades (SPDY
// and WebSocket exec, attach, port-forward) and long-lived watches and log
// follows work unchanged. Plain requests could have been HTTP/2 requests of
// their own, but upgrades cannot, and one path for everything is simpler.
//
// client-go's SPDY and WebSocket round trippers ignore rest.Config.Dial
// (they dial with their own net.Dialer), so the console listens on a
// loopback port per connected cluster and hands out rest.Configs pointing
// there. That port is only reachable inside the console's pod, and the agent
// additionally requires a per-session key (the bearer token of those
// configs), so another process there cannot use it. The agent replaces the
// key with its own service account's credentials.
//
// The tunnel carries requests to the cluster's API server only: the agent's
// handler has no other destination. Health and join material travel as a
// plain HTTP/2 request from the console to the agent (GET InfoPath).

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	// ConnectPath is where agents dial in on the console.
	ConnectPath = "/api/v1/clusters/connect"
	// InfoPath answers the agent's health and join material (AgentInfo),
	// asked by the console over the tunnel.
	InfoPath = "/kwerft-agent/v1/info"

	// AgentTokenPrefix starts every agent token: "kwag_<cluster>_<secret>".
	AgentTokenPrefix = "kwag_"

	// Secrets in the management cluster's kwerft-system namespace, owned by
	// the Cluster. See docs/phase5.md, "Join material".
	//
	// cluster-<name>-agent exists only while a hetzner-cloud cluster has
	// not connected yet: SecretToken and SecretConsoleURL for its first
	// server's cloud-init (install.sh --agent). Adopted clusters never store
	// their token; its hash is the Cluster's TokenHashAnnotation.
	SecretToken      = "token"
	SecretConsoleURL = "consoleURL"
	// cluster-<name>-join: SecretJoinServer and SecretJoinToken (the k3s
	// join token), published by the agent.
	SecretJoinServer = "server"
	SecretJoinToken  = "token"

	// LocalJoinSecret is the join material of a cluster as the installer
	// writes it in that cluster's own kwerft-system namespace (keys
	// SecretJoinServer and SecretJoinToken); an agent publishes it to the
	// console as AgentInfo.Join.
	LocalJoinSecret = "cluster-local-join"
)

// AgentSecretName is the Secret holding a cluster's agent token hash.
func AgentSecretName(cluster string) string { return "cluster-" + cluster + "-agent" }

// JoinSecretName is the Secret holding a cluster's join material.
func JoinSecretName(cluster string) string { return "cluster-" + cluster + "-join" }

// MaxNameLength bounds cluster names: they end up in Secret, NodePool and
// server names and in agent tokens.
const MaxNameLength = 40

var nameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidName reports whether name can name a cluster: a DNS label of at most
// MaxNameLength characters, starting with a letter.
func ValidName(name string) bool {
	return len(name) <= MaxNameLength && nameRE.MatchString(name)
}

// NewAgentToken returns a fresh token for cluster's agent.
func NewAgentToken(cluster string) string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return AgentTokenPrefix + cluster + "_" + base64.RawURLEncoding.EncodeToString(b)
}

// AgentTokenCluster returns the cluster an agent token names.
func AgentTokenCluster(token string) (string, bool) {
	rest, ok := strings.CutPrefix(token, AgentTokenPrefix)
	if !ok {
		return "", false
	}
	name, secret, ok := strings.Cut(rest, "_")
	if !ok || len(secret) < 32 || !ValidName(name) || name == Local {
		return "", false
	}
	return name, true
}

// AgentInfo is what an agent reports about its cluster.
type AgentInfo struct {
	AgentVersion      string `json:"agentVersion"`
	KubernetesVersion string `json:"kubernetesVersion"`
	Nodes             int32  `json:"nodes"`
	ReadyNodes        int32  `json:"readyNodes"`
	// Join is the cluster's join material, once the installer wrote it.
	Join *JoinMaterial `json:"join,omitempty"`
	// Error says what the agent could not find out (the API server was
	// unreachable, say); the rest is then partial.
	Error string `json:"error,omitempty"`
	// ProxyKey authenticates the console's requests to the agent's API
	// proxy for this session. Never stored or shown.
	ProxyKey string `json:"proxyKey,omitempty"`
}

// JoinMaterial lets a new node join the cluster (k3s).
type JoinMaterial struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

// streamConn is a net.Conn over one HTTP/2 CONNECT stream. It has no
// deadlines; both ends only copy bytes through it.
type streamConn struct {
	r    io.ReadCloser
	w    io.Writer
	wc   io.Closer // closes the write side; nil when closing r is enough
	once sync.Once
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.r.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.w.Write(p) }
func (c *streamConn) Close() error {
	c.once.Do(func() {
		if c.wc != nil {
			_ = c.wc.Close()
		}
		_ = c.r.Close()
	})
	return nil
}

// pipe copies a and b into each other until either side ends, then closes
// both.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}

// chanListener is a net.Listener whose connections are handed to it.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
	addr  net.Addr
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{}), addr: tunnelAddr{}}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

// hand passes c to Accept; false once the listener is closed.
func (l *chanListener) hand(ctx context.Context, c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		return false
	case <-ctx.Done():
		return false
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return l.addr }

type tunnelAddr struct{}

func (tunnelAddr) Network() string { return "tunnel" }
func (tunnelAddr) String() string  { return "tunnel" }

// backoff doubles from min to max.
type backoff struct {
	min, max, cur time.Duration
}

func (b *backoff) next() time.Duration {
	if b.cur < b.min {
		b.cur = b.min
	} else {
		b.cur *= 2
	}
	if b.cur > b.max {
		b.cur = b.max
	}
	// ±20% jitter, so agents of a restarted console do not dial in lockstep.
	var j [1]byte
	_, _ = rand.Read(j[:])
	return b.cur + time.Duration(int64(b.cur)/5*(int64(j[0])-128)/128)
}

func (b *backoff) reset() { b.cur = 0 }
