package clusters

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
)

// fakeAPI stands in for a remote cluster's API server: TLS with HTTP/2 (as
// a real one), the agent's bearer token, and the few endpoints the tests
// stream through.
type fakeAPI struct {
	srv     *httptest.Server
	release chan struct{} // lets the log stream's second line go

	mu    sync.Mutex
	seen  []http.Header // headers of every request
	proto []string      // protocol of every request, by path
}

const agentSAToken = "agent-service-account-token"

func newFakeAPI(t *testing.T) *fakeAPI {
	f := &fakeAPI{release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": "v1.37.1+k3s1"})
	})
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		ready := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue}
		notReady := corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionFalse}
		_ = json.NewEncoder(w).Encode(corev1.NodeList{TypeMeta: metav1.TypeMeta{Kind: "NodeList", APIVersion: "v1"}, Items: []corev1.Node{
			{ObjectMeta: metav1.ObjectMeta{Name: "a"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{ready}}},
			{ObjectMeta: metav1.ObjectMeta{Name: "b"}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{notReady}}},
		}})
	})
	mux.HandleFunc("GET /api/v1/namespaces/kwerft-system/secrets/cluster-local-join", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(corev1.Secret{TypeMeta: metav1.TypeMeta{Kind: "Secret", APIVersion: "v1"},
			ObjectMeta: metav1.ObjectMeta{Name: LocalJoinSecret, Namespace: "kwerft-system"},
			Data:       map[string][]byte{SecretJoinServer: []byte("https://10.0.0.2:6443"), SecretJoinToken: []byte("K10join::server:secret")}})
	})
	// A log follow: the first line now, the second when the test says so.
	mux.HandleFunc("GET /api/v1/namespaces/default/pods/web/log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "line 1\n")
		http.NewResponseController(w).Flush()
		select {
		case <-f.release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "line 2\n")
	})
	// exec over WebSocket (the v5 channel protocol client-go speaks):
	// stdin comes back on stdout.
	mux.HandleFunc("GET /api/v1/namespaces/default/pods/web/exec", func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil || len(msg) == 0 {
				return
			}
			switch msg[0] {
			case 0: // stdin
				_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{1}, msg[1:]...))
			case 255: // stdin closed
				_ = c.WriteMessage(websocket.BinaryMessage, append([]byte{3}, `{"metadata":{},"status":"Success"}`...))
				_ = c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				return
			}
		}
	})
	// Any other upgrade (SPDY, as kubectl exec and port-forward use): an
	// echo after 101. Only possible over HTTP/1.1.
	mux.HandleFunc("GET /upgrade/echo", func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			http.Error(w, "upgrade over "+r.Proto, http.StatusHTTPVersionNotSupported)
			return
		}
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	})
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+agentSAToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.seen = append(f.seen, r.Header.Clone())
		f.proto = append(f.proto, r.URL.Path+" "+r.Proto)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		mux.ServeHTTP(w, r)
	}))
	f.srv.EnableHTTP2 = true
	f.srv.StartTLS()
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) config() *rest.Config {
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return &rest.Config{Host: f.srv.URL, BearerToken: agentSAToken, TLSClientConfig: rest.TLSClientConfig{CAData: cert}}
}

func (f *fakeAPI) headers() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.seen...)
}

// testConsole is the console's tunnel endpoint over TLS, counting refusals.
type testConsole struct {
	hub      *Hub
	srv      *httptest.Server
	clusters client.Client
	rejected atomic.Int32
}

func newTestConsole(t *testing.T, objs ...client.Object) *testConsole {
	scheme := runtime.NewScheme()
	if err := kwerftv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := &testConsole{clusters: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()}
	c.hub = &Hub{Local: &rest.Config{Host: "https://management"}, Clusters: c.clusters, Logger: slog.New(slog.DiscardHandler), InfoInterval: 100 * time.Millisecond}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ConnectPath {
			http.NotFound(w, r)
			return
		}
		if err := c.hub.Connect(w, r); errors.Is(err, ErrRejected) {
			c.rejected.Add(1)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *testConsole) startAgent(t *testing.T, api *rest.Config, token func() (string, error)) context.CancelFunc {
	pool := x509.NewCertPool()
	pool.AddCert(c.srv.Certificate())
	a := &Agent{
		ConsoleURL: c.srv.URL, Token: token, API: api, Namespace: "kwerft-system",
		TLS: &tls.Config{RootCAs: pool}, Logger: slog.New(slog.DiscardHandler),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, Rejected: 100 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = a.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return cancel
}

func adoptedCluster(name, token string) *kwerftv1.Cluster {
	return &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{TokenHashAnnotation: auth.HashToken(token)}},
		Spec:       kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func connected(h *Hub, name string) func() bool {
	return func() bool { _, ok := h.Agent(name); return ok }
}

func TestAgentTokens(t *testing.T) {
	tok := NewAgentToken("edge-1")
	if name, ok := AgentTokenCluster(tok); !ok || name != "edge-1" {
		t.Errorf("AgentTokenCluster(%q) = %q %v", tok, name, ok)
	}
	for _, bad := range []string{"", "kwag_", "kwag_edge", "kwft_edge_" + strings.Repeat("a", 43), "kwag_local_" + strings.Repeat("a", 43),
		"kwag_Edge_" + strings.Repeat("a", 43), "kwag_edge_short"} {
		if _, ok := AgentTokenCluster(bad); ok {
			t.Errorf("AgentTokenCluster(%q) accepted", bad)
		}
	}
	for name, want := range map[string]bool{"edge": true, "a": true, "edge-1": true, "1edge": false, "edge-": false, "Edge": false,
		"a.b": false, strings.Repeat("a", MaxNameLength): true, strings.Repeat("a", MaxNameLength+1): false} {
		if ValidName(name) != want {
			t.Errorf("ValidName(%q) = %v", name, !want)
		}
	}
}

func TestTunnelCarriesStreamsAndUpgrades(t *testing.T) {
	api := newFakeAPI(t)
	token := NewAgentToken("edge")
	c := newTestConsole(t, adoptedCluster("edge", token), &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "away"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}})
	changed := c.hub.Changed()
	c.startAgent(t, api.config(), func() (string, error) { return token, nil })
	waitFor(t, "the agent", connected(c.hub, "edge"))
	select {
	case <-changed:
	default:
		t.Error("Changed was not closed on connect")
	}

	// Registry view.
	list, err := c.hub.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(list) != "[{local true} {away false} {edge true}]" {
		t.Errorf("List = %v", list)
	}
	if _, err := c.hub.RESTConfig("away"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RESTConfig(away) = %v, want ErrUnavailable", err)
	}
	if _, err := c.hub.RESTConfig("nowhere"); !errors.Is(err, ErrUnknown) {
		t.Errorf("RESTConfig(nowhere) = %v, want ErrUnknown", err)
	}
	if cfg, err := c.hub.RESTConfig(Local); err != nil || cfg.Host != "https://management" {
		t.Errorf("RESTConfig(local) = %v %v", cfg, err)
	}

	// Health and join material.
	st, _ := c.hub.Agent("edge")
	if st.Info.KubernetesVersion != "v1.37.1+k3s1" || st.Info.Nodes != 2 || st.Info.ReadyNodes != 1 || st.Info.ProxyKey != "" {
		t.Errorf("agent status = %+v", st.Info)
	}
	if st.Info.Join == nil || st.Info.Join.Server != "https://10.0.0.2:6443" || st.Info.Join.Token != "K10join::server:secret" {
		t.Errorf("join material = %+v", st.Info.Join)
	}

	cfg, err := c.hub.RESTConfig("edge")
	if err != nil {
		t.Fatal(err)
	}
	cs := kubernetes.NewForConfigOrDie(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A plain request, with the user's impersonation passed on and the
	// agent's credentials in place of the session key.
	imp := rest.CopyConfig(cfg)
	imp.Impersonate = rest.ImpersonationConfig{UserName: "kwerft:dev@example.com", Groups: []string{"kwerft:role:developer"}}
	if _, err := kubernetes.NewForConfigOrDie(imp).CoreV1().Nodes().List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("list nodes through the tunnel: %v", err)
	}
	h := api.headers()
	last := h[len(h)-1]
	if last.Get("Impersonate-User") != "kwerft:dev@example.com" || last.Get("Impersonate-Group") != "kwerft:role:developer" {
		t.Errorf("impersonation not passed on: %v", last)
	}

	// A log follow streams: the first line arrives while the second is held.
	stream, err := cs.CoreV1().Pods("default").GetLogs("web", &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(stream)
	if l, err := lines.ReadString('\n'); err != nil || l != "line 1\n" {
		t.Fatalf("first log line = %q %v", l, err)
	}
	close(api.release)
	if l, err := lines.ReadString('\n'); err != nil || l != "line 2\n" {
		t.Fatalf("second log line = %q %v", l, err)
	}
	_ = stream.Close()

	// exec over WebSocket, as the console's shells do.
	execURL := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace("default").Name("web").SubResource("exec").
		Param("command", "cat").Param("stdin", "true").Param("stdout", "true").URL()
	ex, err := remotecommand.NewWebSocketExecutor(cfg, "GET", execURL.String())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := ex.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: strings.NewReader("hello through the tunnel\n"), Stdout: &out}); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if out.String() != "hello through the tunnel\n" {
		t.Errorf("exec output = %q", out.String())
	}

	// Any other upgrade (SPDY) goes to the API server over HTTP/1.1.
	conn := rawUpgrade(t, cfg, "/upgrade/echo", "Bearer "+cfg.BearerToken)
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "ping" {
		t.Errorf("upgraded echo = %q %v", buf, err)
	}
	_ = conn.Close()
	api.mu.Lock()
	protos := strings.Join(api.proto, "\n")
	api.mu.Unlock()
	if !strings.Contains(protos, "/upgrade/echo HTTP/1.1") || !strings.Contains(protos, "/api/v1/nodes HTTP/2.0") {
		t.Errorf("protocols to the API server:\n%s", protos)
	}

	// The loopback end refuses requests without the session key, so other
	// processes in the console's pod cannot use the agent's identity.
	for _, authz := range []string{"", "Bearer wrong", "Bearer " + agentSAToken} {
		req, _ := http.NewRequest("GET", cfg.Host+"/api/v1/nodes", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("loopback with %q: %d, want 401", authz, res.StatusCode)
		}
	}
	if c.rejected.Load() != 0 {
		t.Errorf("%d agents refused", c.rejected.Load())
	}
}

// rawUpgrade sends an upgrade request straight to the tunnel's loopback end
// and returns the connection after the 101.
func rawUpgrade(t *testing.T, cfg *rest.Config, path, authz string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(cfg.Host, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: kubernetes\r\nAuthorization: %s\r\nConnection: Upgrade\r\nUpgrade: SPDY/3.1\r\n\r\n", path, authz)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(res.Body)
		t.Fatalf("upgrade: %s %s", res.Status, body)
	}
	return conn
}

func TestTunnelRefusesBadTokens(t *testing.T) {
	api := newFakeAPI(t)
	token := NewAgentToken("edge")
	deleting := adoptedCluster("gone", NewAgentToken("gone"))
	c := newTestConsole(t, adoptedCluster("edge", token), &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "nohash"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}, deleting)

	for _, tok := range []string{
		NewAgentToken("edge"),    // right cluster, wrong secret
		NewAgentToken("nowhere"), // no such cluster
		NewAgentToken("nohash"),  // a cluster without a token
		"kwft_" + token,          // not an agent token
	} {
		c.startAgent(t, api.config(), func() (string, error) { return tok, nil })
	}
	waitFor(t, "refusals", func() bool { return c.rejected.Load() >= 4 })
	list, _ := c.hub.List(context.Background())
	for _, i := range list[1:] {
		if i.Connected {
			t.Errorf("%s connected with a bad token", i.Name)
		}
	}

	// A browser page cannot connect, even with the right token.
	req, _ := http.NewRequest("GET", c.srv.URL+ConnectPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Origin", "https://evil.example")
	res, err := c.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("connect with an Origin: %d, want 403", res.StatusCode)
	}
}

func TestTunnelReconnectsAndRotates(t *testing.T) {
	api := newFakeAPI(t)
	oldToken, newToken := NewAgentToken("edge"), NewAgentToken("edge")
	c := newTestConsole(t, adoptedCluster("edge", oldToken))
	var current atomic.Value
	current.Store(oldToken)
	c.startAgent(t, api.config(), func() (string, error) { return current.Load().(string), nil })
	waitFor(t, "the agent", connected(c.hub, "edge"))
	first, _ := c.hub.RESTConfig("edge")

	// The console drops the connection: the agent comes back by itself, on
	// a new session (new loopback port, new key).
	changed := c.hub.Changed()
	c.hub.Disconnect("edge")
	<-changed
	waitFor(t, "the agent to reconnect", connected(c.hub, "edge"))
	second, _ := c.hub.RESTConfig("edge")
	if second.Host == first.Host || second.BearerToken == first.BearerToken {
		t.Error("reconnect reused the old session's endpoint or key")
	}
	if _, err := kubernetes.NewForConfigOrDie(first).Discovery().ServerVersion(); err == nil {
		t.Error("the old session's config still works")
	}
	if v, err := kubernetes.NewForConfigOrDie(second).Discovery().ServerVersion(); err != nil || v.GitVersion != "v1.37.1+k3s1" {
		t.Errorf("new session: %v %v", v, err)
	}

	// Rotation: the old token stops working at once, the new one connects.
	var cl kwerftv1.Cluster
	if err := c.clusters.Get(context.Background(), client.ObjectKey{Name: "edge"}, &cl); err != nil {
		t.Fatal(err)
	}
	cl.Annotations[TokenHashAnnotation] = auth.HashToken(newToken)
	if err := c.clusters.Update(context.Background(), &cl); err != nil {
		t.Fatal(err)
	}
	c.hub.DisconnectUnless("edge", auth.HashToken(oldToken)) // still valid: stays
	if _, ok := c.hub.Agent("edge"); !ok {
		t.Fatal("DisconnectUnless dropped a session with a valid token")
	}
	c.hub.DisconnectUnless("edge", auth.HashToken(newToken))
	waitFor(t, "a refusal of the old token", func() bool { return c.rejected.Load() > 0 })
	if _, ok := c.hub.Agent("edge"); ok {
		t.Fatal("connected with the rotated-out token")
	}
	current.Store(newToken)
	waitFor(t, "the agent with the new token", connected(c.hub, "edge"))
}

func TestTunnelReplacesOlderConnection(t *testing.T) {
	api := newFakeAPI(t)
	token := NewAgentToken("edge")
	c := newTestConsole(t, adoptedCluster("edge", token))
	c.startAgent(t, api.config(), func() (string, error) { return token, nil })
	waitFor(t, "the first agent", connected(c.hub, "edge"))
	first, _ := c.hub.Agent("edge")

	// A second agent with the same token takes over; never two sessions.
	c.startAgent(t, api.config(), func() (string, error) { return token, nil })
	waitFor(t, "the second agent", func() bool { st, ok := c.hub.Agent("edge"); return ok && st.Since.After(first.Since) })
	c.hub.mu.Lock()
	n := len(c.hub.sessions)
	c.hub.mu.Unlock()
	if n != 1 {
		t.Errorf("%d sessions, want 1", n)
	}
}
