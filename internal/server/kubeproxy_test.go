// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/kube"
)

// fakeAPIServer records what reaches "Kubernetes" through the proxy.
type fakeAPIServer struct {
	srv     *httptest.Server
	mu      sync.Mutex
	reqs    []*http.Request
	release chan struct{} // ends a watch
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	f := &fakeAPIServer{release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(r.Context()))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("watch") == "true" {
			_, _ = io.WriteString(w, `{"type":"ADDED"}`+"\n")
			w.(http.Flusher).Flush()
			select {
			case <-f.release:
			case <-r.Context().Done():
			}
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = io.WriteString(w, `{"kind":"Status","path":"`+r.URL.Path+`"}`)
	}))
	t.Cleanup(func() { close(f.release); f.srv.Close() })
	return f
}

func (f *fakeAPIServer) last(t *testing.T) *http.Request {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("nothing reached the API server")
	}
	return f.reqs[len(f.reqs)-1]
}

func (f *fakeAPIServer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func proxyEnv(t *testing.T) (*env, *fakeAPIServer) {
	t.Helper()
	up := newFakeAPIServer(t)
	imp, err := kube.NewImpersonator(&rest.Config{Host: up.srv.URL, BearerToken: "console-service-account"}, nil,
		meta.NewDefaultRESTMapper(nil), controllers.NewScheme())
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *Config) { c.Kube = imp })
	e.completeSetup(t)
	return e, up
}

// kubectl sends a request through the proxy like kubectl would.
func (e *env) kubectl(t *testing.T, tok, method, path, body string, hdr ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+"/k8s"+path, strings.NewReader(body))
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Add(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestKubeProxyImpersonatesTheTokensUser(t *testing.T) {
	e, up := proxyEnv(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "kubectl", "role": "viewer"})
	code, body := e.kubectl(t, tok, "GET", "/api/v1/namespaces/shop/pods?limit=500", "", "Cookie", "kwerft_session=abc", "X-Forwarded-For", "1.2.3.4")
	if code != http.StatusOK || !strings.Contains(body, "/api/v1/namespaces/shop/pods") {
		t.Fatalf("get pods: %d %s", code, body)
	}
	r := up.last(t)
	if r.URL.Path != "/api/v1/namespaces/shop/pods" || r.URL.RawQuery != "limit=500" {
		t.Errorf("forwarded %s?%s", r.URL.Path, r.URL.RawQuery)
	}
	if got := r.Header.Get("Authorization"); got != "Bearer console-service-account" {
		t.Errorf("credentials sent upstream: %q", got)
	}
	if got := r.Header.Get("Impersonate-User"); got != "kwerft:"+owner["email"] {
		t.Errorf("Impersonate-User %q", got)
	}
	if got := r.Header.Values("Impersonate-Group"); strings.Join(got, ",") != "kwerft:role:viewer,system:authenticated" {
		t.Errorf("Impersonate-Group %v (the token's cap, not the owner role)", got)
	}
	for _, h := range []string{"Cookie", "X-Forwarded-For"} {
		if r.Header.Get(h) != "" {
			t.Errorf("%s forwarded: %q", h, r.Header.Get(h))
		}
	}
	for name, v := range r.Header {
		if strings.Contains(strings.Join(v, ","), "kwft_") {
			t.Errorf("the API token reached the API server in %s", name)
		}
	}
}

func TestKubeProxyRefusals(t *testing.T) {
	e, up := proxyEnv(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "kubectl"})
	for _, c := range []struct {
		name, method, path string
		hdr                []string
		want               int
	}{
		{"no token", "GET", "/api/v1/pods", nil, http.StatusUnauthorized},
		{"kubectl --as", "GET", "/api/v1/pods", []string{"Impersonate-User", "system:admin"}, http.StatusForbidden},
		{"kubectl --as-group", "GET", "/api/v1/pods", []string{"Impersonate-Group", "system:masters"}, http.StatusForbidden},
		{"impersonate extra", "GET", "/api/v1/pods", []string{"Impersonate-Extra-scopes", "x"}, http.StatusForbidden},
		{"exec", "POST", "/api/v1/namespaces/shop/pods/web-1/exec?command=sh", nil, http.StatusForbidden},
		{"attach", "POST", "/api/v1/namespaces/shop/pods/web-1/attach", nil, http.StatusForbidden},
		{"port-forward", "POST", "/api/v1/namespaces/shop/pods/web-1/portforward", nil, http.StatusForbidden},
		{"pod proxy", "GET", "/api/v1/namespaces/shop/pods/web-1/proxy/admin", nil, http.StatusForbidden},
		{"service proxy", "GET", "/api/v1/namespaces/shop/services/web/proxy/", nil, http.StatusForbidden},
		{"node proxy", "GET", "/api/v1/nodes/n1/proxy/metrics", nil, http.StatusForbidden},
		{"upgrade", "GET", "/api/v1/namespaces/shop/pods", []string{"Connection", "Upgrade", "Upgrade", "websocket"}, http.StatusForbidden},
		{"secrets", "GET", "/api/v1/namespaces/shop/secrets", nil, http.StatusForbidden},
		{"secret patch (would echo the data)", "PATCH", "/api/v1/namespaces/kwerft-system/secrets/kwerft-dns-token", nil, http.StatusForbidden},
		{"all secrets", "GET", "/api/v1/secrets", nil, http.StatusForbidden},
		{"encoded slash", "GET", "/api/v1/namespaces/shop%2Fx/pods", nil, http.StatusForbidden},
		{"legacy watch prefix", "GET", "/api/v1/watch/namespaces/shop/pods", nil, http.StatusForbidden},
		{"not the Kubernetes API", "GET", "/metrics", nil, http.StatusForbidden},
	} {
		before := up.count()
		tk := tok
		if c.name == "no token" {
			tk = ""
		}
		code, body := e.kubectl(t, tk, c.method, c.path, "", c.hdr...)
		if code != c.want {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
		if code == http.StatusForbidden && !strings.Contains(body, `"kind":"Status"`) {
			t.Errorf("%s: not a Kubernetes Status: %s", c.name, body)
		}
		if up.count() != before {
			t.Errorf("%s: reached the API server", c.name)
		}
	}
	if a := lastAudit(t, e.store, "kube.denied"); a == nil || a.Actor != owner["email"] {
		t.Errorf("denials not audited: %+v", a)
	}
	// A session cookie does not get through either.
	req, _ := http.NewRequest("GET", e.srv.URL+"/k8s/api/v1/pods", nil)
	res, err := e.client.Do(req) // carries the owner's session cookie
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("cookie: %d", res.StatusCode)
	}
}

func TestKubeProxyProjectRestriction(t *testing.T) {
	e, up := proxyEnv(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "shop", "role": "developer", "projects": []string{"shop"}})
	for _, c := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/api", true},
		{"GET", "/apis", true},
		{"GET", "/api/v1", true},
		{"GET", "/apis/apps/v1", true},
		{"GET", "/version", true},
		{"GET", "/openapi/v3/apis/apps/v1", true},
		{"POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", true},
		{"POST", "/apis/authentication.k8s.io/v1/selfsubjectreviews", true},
		{"GET", "/api/v1/namespaces/shop/pods", true},
		{"GET", "/api/v1/namespaces/shop/pods/web-1/log?follow=true", true},
		{"POST", "/apis/kwerft.dev/v1alpha1/namespaces/shop/apps", true},
		{"DELETE", "/apis/kwerft.dev/v1alpha1/namespaces/shop/apps/web", true},
		{"GET", "/api/v1/namespaces/shop", true},
		{"GET", "/apis/kwerft.dev/v1alpha1/projects/shop", true},
		{"DELETE", "/api/v1/namespaces/shop", false},
		{"GET", "/api/v1/namespaces/blog/pods", false},
		{"GET", "/api/v1/namespaces/shop/../blog/pods", false}, // the mux cleans the path first
		{"GET", "/api/v1/namespaces/blog", false},
		{"GET", "/api/v1/pods", false},
		{"GET", "/api/v1/namespaces", false},
		{"GET", "/apis/kwerft.dev/v1alpha1/projects", false},
		{"GET", "/apis/kwerft.dev/v1alpha1/projects/blog", false},
		{"DELETE", "/apis/kwerft.dev/v1alpha1/projects/shop", false},
		{"GET", "/apis/kwerft.dev/v1alpha1/apps", false},
		{"GET", "/api/v1/nodes", false},
		{"POST", "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings", false},
		{"POST", "/api", false},
	} {
		before := up.count()
		code, body := e.kubectl(t, tok, c.method, c.path, "{}")
		reached := up.count() > before
		if c.ok != reached || (c.ok && code >= 400) || (!c.ok && code != http.StatusForbidden) {
			t.Errorf("%s %s: %d reached=%v %s", c.method, c.path, code, reached, body)
		}
	}
}

func TestKubeProxyStreamsAndAuditsWrites(t *testing.T) {
	e, up := proxyEnv(t)
	tok, _ := e.newAPIToken(t, map[string]any{"name": "ci", "role": "developer"})
	// A watch: the first event arrives while the stream stays open.
	req, _ := http.NewRequest("GET", e.srv.URL+"/k8s/api/v1/namespaces/shop/pods?watch=true", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line := make(chan string, 1)
	go func() {
		s, _ := bufio.NewReader(res.Body).ReadString('\n')
		line <- s
	}()
	select {
	case s := <-line:
		if !strings.Contains(s, "ADDED") {
			t.Errorf("watch event %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch event not flushed through the proxy")
	}
	res.Body.Close()

	code, _ := e.kubectl(t, tok, "POST", "/apis/kwerft.dev/v1alpha1/namespaces/shop/apps", `{"kind":"App"}`)
	if code != http.StatusCreated || up.last(t).Method != "POST" {
		t.Fatalf("create: %d", code)
	}
	// No waiting: the write is on the record before kubectl has its answer.
	a := lastAudit(t, e.store, "kube.write")
	if a == nil || a.Target != "POST /apis/kwerft.dev/v1alpha1/namespaces/shop/apps" || !strings.Contains(a.Detail, "ci") || !strings.Contains(a.Detail, "Created") {
		t.Errorf("write audit %+v", a)
	}
	// Self-reviews are no writes.
	n := countAudit(t, e, "kube.write")
	e.kubectl(t, tok, "POST", "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "{}")
	if countAudit(t, e, "kube.write") != n {
		t.Error("a self-review was audited as a write")
	}
}

func countAudit(t *testing.T, e *env, action string) int {
	t.Helper()
	list, _ := e.store.RecentAudit(t.Context(), 500)
	n := 0
	for _, a := range list {
		if a.Action == action {
			n++
		}
	}
	return n
}

func TestKubeconfigDownload(t *testing.T) {
	e, up := proxyEnv(t)
	code, out := e.call(t, "POST", "/api/v1/account/kubeconfig", map[string]any{"role": "viewer", "projects": []string{"shop"}}, nil)
	if code != http.StatusCreated {
		t.Fatalf("kubeconfig: %d %v", code, out)
	}
	var cfg struct {
		Clusters []struct {
			Cluster struct{ Server string } `json:"cluster"`
		} `json:"clusters"`
		Users []struct {
			User struct{ Token string } `json:"user"`
		} `json:"users"`
		Contexts []struct {
			Context struct{ Namespace string } `json:"context"`
		} `json:"contexts"`
	}
	if err := yaml.Unmarshal([]byte(out["kubeconfig"].(string)), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clusters) != 1 || cfg.Clusters[0].Cluster.Server != e.srv.URL+"/k8s" || cfg.Contexts[0].Context.Namespace != "shop" {
		t.Errorf("kubeconfig %+v", cfg)
	}
	if info := out["apiToken"].(map[string]any); info["kind"] != "kubeconfig" || !strings.HasPrefix(info["name"].(string), "kubeconfig ") {
		t.Errorf("token %v", info)
	}
	// The embedded token works with the proxy.
	if code, _ := e.kubectl(t, cfg.Users[0].User.Token, "GET", "/api/v1/namespaces/shop/pods", ""); code != http.StatusOK {
		t.Errorf("kubeconfig token: %d", code)
	}
	if got := up.last(t).Header.Values("Impersonate-Group"); got[0] != "kwerft:role:viewer" {
		t.Errorf("groups %v", got)
	}
}
