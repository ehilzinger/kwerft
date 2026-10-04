package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// testCA issues certificates like an ACME CA would; staging CAs put
// "(STAGING)" into the issuer.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func newTestCA(t *testing.T, org string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{Organization: []string{org}, CommonName: org + " Root"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &testCA{cert: cert, key: key}
}

func (ca *testCA) issue(t *testing.T, names ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeConsole is the console and its apps behind one TLS listener, routed
// by Host like Traefik does.
type fakeConsole struct {
	*httptest.Server
	domain string

	mu        sync.Mutex
	version   string // set by the fake installer
	token     string
	owner     *owner
	grant     bool
	sessions  map[string]bool
	projects  map[string]bool
	apps      map[string]kwerftv1.AppSpec // "project/name"
	webGen    int                         // restarts of web
	buildPoll int                         // GETs until the build succeeds
	buildFail bool
	taskDone  bool
	noAlert   bool
}

func newFakeConsole(t *testing.T, ca *testCA, ip string) *fakeConsole {
	f := &fakeConsole{
		domain: ip + ".sslip.io", token: "kwft_setup_test", sessions: map[string]bool{},
		projects: map[string]bool{}, apps: map[string]kwerftv1.AppSpec{}, buildPoll: 2,
	}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issue(t, f.domain, "*."+f.domain)}}
	f.StartTLS()
	t.Cleanup(f.Close)
	return f
}

// client is newHTTPClient with every connection going to the fake.
func (f *fakeConsole) client(certs *certChecker) *http.Client {
	c := newHTTPClient(certs)
	addr := f.Listener.Addr().String()
	c.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return c
}

func (f *fakeConsole) installed(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version = version
}

func (f *fakeConsole) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != f.domain {
		app, _ := strings.CutSuffix(host, "."+f.domain)
		for key, spec := range f.apps {
			if len(spec.Ports) > 0 && spec.Ports[0].Public == host {
				if spec.Source.Git != nil && f.buildPoll > 0 {
					break
				}
				fmt.Fprintf(w, "Hostname: %s-%d\nIP: 10.0.0.1\n", strings.ReplaceAll(key, "/", "-"), f.webGen)
				return
			}
		}
		http.Error(w, "404 page not found "+app, http.StatusNotFound)
		return
	}
	if f.version == "" {
		http.Error(w, "no console yet", http.StatusBadGateway)
		return
	}
	authed := false
	if c, err := r.Cookie("__Host-kwerft_session"); err == nil && f.sessions[c.Value] {
		authed = true
	}
	path := r.URL.Path
	parts := strings.Split(strings.Trim(path, "/"), "/")
	signIn := func() {
		id := randomString(16)
		f.sessions[id] = true
		http.SetCookie(w, &http.Cookie{Name: "__Host-kwerft_session", Value: id, Path: "/", Secure: true, HttpOnly: true})
	}
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	}
	str := func(k string) string { s, _ := body[k].(string); return s }

	switch {
	case path == "/healthz":
		w.WriteHeader(200)
	case path == "/api/v1/version":
		writeJSONTest(w, 200, map[string]string{"version": f.version})
	case path == "/api/v1/setup":
		writeJSONTest(w, 200, map[string]any{"complete": f.owner != nil})
	case path == "/api/v1/setup/verify" && r.Method == "POST":
		if str("token") != f.token || f.owner != nil {
			writeJSONTest(w, 401, map[string]string{"error": "That token doesn't match."})
			return
		}
		f.grant = true
		w.WriteHeader(204)
	case path == "/api/v1/setup/owner" && r.Method == "POST":
		if !f.grant {
			writeJSONTest(w, 401, map[string]string{"error": "Enter the setup token first."})
			return
		}
		f.owner = &owner{Name: str("name"), Email: str("email"), Password: str("password")}
		signIn()
		writeJSONTest(w, 201, map[string]string{"email": f.owner.Email})
	case path == "/api/v1/session" && r.Method == "POST":
		if f.owner == nil || str("email") != f.owner.Email || str("password") != f.owner.Password {
			writeJSONTest(w, 401, map[string]string{"error": "Wrong email or password."})
			return
		}
		signIn()
		writeJSONTest(w, 200, map[string]string{"email": f.owner.Email, "role": "owner"})
	case !authed:
		writeJSONTest(w, 401, map[string]string{"error": "Sign in first."})
	case path == "/api/v1/session":
		writeJSONTest(w, 200, map[string]string{"email": f.owner.Email, "role": "owner"})
	case path == "/api/v1/projects" && r.Method == "POST":
		if f.projects[str("name")] {
			writeJSONTest(w, 409, map[string]string{"error": "exists"})
			return
		}
		f.projects[str("name")] = true
		writeJSONTest(w, 201, map[string]string{"name": str("name")})
	case len(parts) == 5 && parts[4] == "apps" && r.Method == "POST":
		var in struct {
			Name string           `json:"name"`
			Spec kwerftv1.AppSpec `json:"spec"`
		}
		data, _ := json.Marshal(body)
		_ = json.Unmarshal(data, &in)
		if !f.projects[parts[3]] {
			writeJSONTest(w, 403, map[string]string{"error": "no project"})
			return
		}
		f.apps[parts[3]+"/"+in.Name] = in.Spec
		writeJSONTest(w, 201, map[string]string{"name": in.Name})
	case path == "/api/v1/apps":
		out := []appSummary{}
		for key := range f.apps {
			p, n, _ := strings.Cut(key, "/")
			if q := r.URL.Query().Get("project"); q == "" || q == p {
				out = append(out, appSummary{Name: n, Project: p, Phase: "running"})
			}
		}
		writeJSONTest(w, 200, out)
	case len(parts) == 7 && parts[6] == "pods":
		writeJSONTest(w, 200, []pod{{Name: fmt.Sprintf("%s-%d", parts[5], f.webGen), Status: "Running", Ready: true}})
	case len(parts) == 7 && parts[6] == "builds" && r.Method == "POST":
		writeJSONTest(w, 201, build{Name: "git-1", Phase: "pending"})
	case len(parts) == 6 && parts[4] == "builds":
		b := build{Name: parts[5], Phase: "running"}
		if f.buildPoll--; f.buildPoll <= 0 {
			b.Phase = "succeeded"
			if f.buildFail {
				b.Phase, b.StatusMessage = "failed", "Dockerfile not found"
				f.buildPoll = 99
			}
		}
		writeJSONTest(w, 200, b)
	case len(parts) == 5 && parts[4] == "tasks" && r.Method == "POST":
		t := kwerftv1.Task{}
		t.Name = "busybox-run-abc12"
		writeJSONTest(w, 201, t)
	case len(parts) == 6 && parts[4] == "tasks":
		t := kwerftv1.Task{}
		t.Name = parts[5]
		t.Status.Phase = kwerftv1.TaskRunning
		if f.taskDone {
			t.Status.Phase = kwerftv1.TaskSucceeded
		} else {
			f.taskDone = true
			f.webGen++
		}
		writeJSONTest(w, 200, t)
	case path == "/api/v1/logs":
		out := []logEntry{}
		if f.taskDone {
			out = append(out, logEntry{Project: "e2e", Task: "busybox-run-abc12", Line: r.URL.Query().Get("query")})
		}
		writeJSONTest(w, 200, map[string]any{"entries": out})
	case path == "/api/v1/metrics/query":
		writeJSONTest(w, 200, map[string]any{"series": []map[string]any{{"labels": map[string]string{"namespace": "e2e"}, "points": []any{[]any{1, 2}}}}})
	case path == "/api/v1/alerts":
		out := []alert{}
		if _, ok := f.apps["e2e/crash"]; ok && !f.noAlert {
			out = append(out, alert{Rule: "crash-looping", State: "firing", Project: "e2e", App: "crash", Summary: "A container keeps crashing"})
		}
		writeJSONTest(w, 200, out)
	default:
		writeJSONTest(w, 404, map[string]string{"error": "not found " + r.Method + " " + path})
	}
}
