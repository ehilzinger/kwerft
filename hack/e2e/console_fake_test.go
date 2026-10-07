// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

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
	"regexp"
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

// fakeNode is a node of the fake cluster.
type fakeNode struct {
	name, role, version string
	unschedulable       bool
}

// fakeUpgrade is an Upgrade the fake runs through its phases, one step
// per event the stream sends.
type fakeUpgrade struct {
	view    upgradeView
	script  []func(*fakeConsole, *upgradeView)
	pos     int
	fault   string
	dropAt  int // the event stream breaks once after this step (the console restarts)
	dropped bool
}

// fakeConsole is the console and its apps behind one TLS listener, routed
// by Host like Traefik does.
type fakeConsole struct {
	*httptest.Server
	domain string

	mu        sync.Mutex
	version   string // set by the fake installer
	down      bool   // the server is gone
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
	dialed    []string // addresses the clients connected to

	// tasks by name: their spec and how often they were read.
	tasks map[string]*fakeTask
	// volumes and the files Tasks wrote into them: "project/volume" → file → content.
	volumes map[string]map[string]string
	// secret sets: "project/set" → key → value.
	secrets map[string]map[string]string
	// channel is Settings › Updates' channel ("" is stable).
	channel string

	// upgrades
	upgrades      map[string]*fakeUpgrade
	faults        bool // chart value e2e.faults
	installBase   string
	preflightFail string // the preflight blocks with this message
	failUpgrade   bool   // an upgrade rolls back without a fault
	touchApps     bool   // a failed upgrade restarts hello
	appFail       int    // the next app requests that fail (an App away)
	upgradeLog    string

	// nodes
	nodes       []fakeNode
	joinTokens  map[string]bool
	rejectJoins bool

	// backups
	s3           *fakeS3
	target       *backupTarget
	recoveryKey  string
	targetPolls  int
	backupList   []backupView
	backupPolls  map[string]int
	restoreCheck func(uploads map[string]string) error
}

type fakeTask struct {
	spec  kwerftv1.TaskSpec
	reads int
}

func newFakeConsole(t *testing.T, ca *testCA, ip string) *fakeConsole {
	f := &fakeConsole{
		domain: ip + ".sslip.io", token: "kwft_setup_test", sessions: map[string]bool{},
		projects: map[string]bool{}, apps: map[string]kwerftv1.AppSpec{}, buildPoll: 2,
		tasks: map[string]*fakeTask{}, volumes: map[string]map[string]string{}, secrets: map[string]map[string]string{},
		upgrades: map[string]*fakeUpgrade{}, joinTokens: map[string]bool{}, backupPolls: map[string]int{},
	}
	f.Server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.TLS = &tls.Config{Certificates: []tls.Certificate{ca.issue(t, f.domain, "*."+f.domain)}}
	f.StartTLS()
	t.Cleanup(f.Close)
	return f
}

// client is newHTTPClient with every connection going to the fake; the
// address asked for is recorded.
func (f *fakeConsole) client(certs *certChecker) *http.Client {
	c := newHTTPClient(certs)
	addr := f.Listener.Addr().String()
	c.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, asked string) (net.Conn, error) {
		f.mu.Lock()
		f.dialed = append(f.dialed, asked)
		f.mu.Unlock()
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	return c
}

func (f *fakeConsole) installed(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version, f.down = version, false
}

// gone: the console's server was deleted.
func (f *fakeConsole) gone() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.down = true
}

func (f *fakeConsole) state() (version string, down bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.version, f.down
}

// restored is the console after install.sh --restore: everything but the
// sessions came back from the backup.
func (f *fakeConsole) restored(version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.version, f.down, f.sessions = version, false, map[string]bool{}
}

// addNode is a server joining (or the first one installing) the cluster.
func (f *fakeConsole) addNode(name, role, version string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.nodes {
		if n.name == name {
			return
		}
	}
	f.nodes = append(f.nodes, fakeNode{name: name, role: role, version: version})
}

// spendJoinToken: a join presents its token (GET /api/v1/join).
func (f *fakeConsole) spendJoinToken(tok string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := f.joinTokens[tok]
	delete(f.joinTokens, tok)
	return ok && !f.rejectJoins
}

// setFault annotates an upgrade; honoured only with e2e.faults.
func (f *fakeConsole) setFault(name, fault string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.upgrades[name]
	if !ok {
		return fmt.Errorf("upgrades.kwerft.dev %q not found", name)
	}
	if !f.faults {
		return fmt.Errorf("runner %s never carried --fault=%s", runnerJobName(name), fault)
	}
	u.fault = fault
	return nil
}

var markerCmdRE = regexp.MustCompile(`^echo (\S+) >/www/marker\.txt`)

func (f *fakeConsole) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/events") {
		f.serveEvents(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if f.down {
		http.Error(w, "connection refused", http.StatusBadGateway)
		return
	}
	if host != f.domain {
		f.serveApp(w, r, host)
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
	var raw []byte
	var body map[string]any
	if r.Body != nil {
		raw, _ = io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
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
		_ = json.Unmarshal(raw, &in)
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
		// As the console answers (api_pods.go).
		writeJSONTest(w, 200, map[string]any{"pods": []pod{{Name: fmt.Sprintf("%s-%d", parts[5], f.webGen), Status: "Running", Ready: true}},
			"metrics": true, "access": map[string]bool{"logs": true, "exec": false}})
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
		var in struct {
			Spec kwerftv1.TaskSpec `json:"spec"`
		}
		_ = json.Unmarshal(raw, &in)
		t := kwerftv1.Task{}
		t.Name = fmt.Sprintf("busybox-run-%d", len(f.tasks)+1)
		f.tasks[parts[3]+"/"+t.Name] = &fakeTask{spec: in.Spec}
		writeJSONTest(w, 201, t)
	case len(parts) == 6 && parts[4] == "tasks":
		ft, ok := f.tasks[parts[3]+"/"+parts[5]]
		if !ok {
			writeJSONTest(w, 404, map[string]string{"error": "no task"})
			return
		}
		t := kwerftv1.Task{}
		t.Name = parts[5]
		t.Status.Phase = kwerftv1.TaskRunning
		if ft.reads++; ft.reads > 1 {
			t.Status.Phase = kwerftv1.TaskSucceeded
		} else {
			f.taskDone = true
			if ft.spec.OnSuccess != nil && len(ft.spec.OnSuccess.Restart) > 0 {
				f.webGen++
			}
			// A Task writing into a Volume.
			if len(ft.spec.Volumes) > 0 && len(ft.spec.Command) == 3 {
				if m := markerCmdRE.FindStringSubmatch(ft.spec.Command[2]); m != nil {
					vol := parts[3] + "/" + ft.spec.Volumes[0].Volume
					if f.volumes[vol] == nil {
						t.Status.Phase = kwerftv1.TaskFailed
						writeJSONTest(w, 200, t)
						return
					}
					f.volumes[vol]["marker.txt"] = m[1] + "\n"
				}
			}
		}
		writeJSONTest(w, 200, t)
	case len(parts) == 5 && parts[4] == "volumes" && r.Method == "POST":
		key := parts[3] + "/" + str("name")
		if f.volumes[key] != nil {
			writeJSONTest(w, 409, map[string]string{"error": "exists"})
			return
		}
		f.volumes[key] = map[string]string{}
		writeJSONTest(w, 201, map[string]string{"name": str("name")})
	case len(parts) == 5 && parts[4] == "secret-sets" && r.Method == "POST":
		key := parts[3] + "/" + str("name")
		if f.secrets[key] != nil {
			writeJSONTest(w, 409, map[string]string{"error": "exists"})
			return
		}
		f.secrets[key] = map[string]string{}
		writeJSONTest(w, 201, map[string]string{"name": str("name")})
	case len(parts) == 8 && parts[4] == "secret-sets" && parts[6] == "keys" && r.Method == "PUT":
		set := f.secrets[parts[3]+"/"+parts[5]]
		if set == nil {
			writeJSONTest(w, 404, map[string]string{"error": "no set"})
			return
		}
		set[parts[7]] = str("value")
		writeJSONTest(w, 200, map[string]string{"name": parts[7]}) // never the value
	case len(parts) == 6 && parts[4] == "secret-sets" && r.Method == "GET":
		set := f.secrets[parts[3]+"/"+parts[5]]
		if set == nil {
			writeJSONTest(w, 404, map[string]string{"error": "no set"})
			return
		}
		keys := []map[string]string{}
		for k := range set {
			keys = append(keys, map[string]string{"name": k})
		}
		writeJSONTest(w, 200, map[string]any{"name": parts[5], "keys": keys})
	case path == "/api/v1/logs":
		out := []logEntry{}
		if f.taskDone {
			out = append(out, logEntry{Project: "e2e", Task: "busybox-run-1", Line: r.URL.Query().Get("query")})
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
	case path == "/api/v1/settings/updates" && r.Method == "PUT":
		var req struct{ Policy, Channel string }
		_ = json.Unmarshal(raw, &req)
		if req.Policy != "Notify" || (req.Channel != "stable" && req.Channel != "edge") {
			writeJSONTest(w, 400, map[string]string{"error": "bad policy"})
			return
		}
		f.channel = req.Channel
		writeJSONTest(w, 200, map[string]string{"policy": req.Policy, "channel": req.Channel})
	case strings.HasPrefix(path, "/api/v1/upgrades"):
		f.serveUpgrades(w, r, parts, raw)
	case path == "/api/v1/clusters/local/nodes":
		out := []nodeView{}
		for _, n := range f.nodes {
			out = append(out, nodeView{Name: n.name, Roles: []string{n.role}, Ready: true, Status: "Ready", Kubelet: n.version, Unschedulable: n.unschedulable})
		}
		writeJSONTest(w, 200, map[string]any{"cluster": "local", "reachable": true, "nodes": out})
	case path == "/api/v1/clusters/local/join-command" && r.Method == "POST":
		if str("role") != "worker" {
			writeJSONTest(w, 400, map[string]string{"error": "role"})
			return
		}
		tok := "kwft_join_" + randomString(12)
		f.joinTokens[tok] = true
		writeJSONTest(w, 201, map[string]string{"command": "curl -fsSL https://" + f.domain + "/join.sh | sudo bash -s -- --token " + tok + " --role worker", "role": "worker"})
	case path == "/api/v1/settings/backups" || strings.HasPrefix(path, "/api/v1/backups"):
		f.serveBackups(w, r, path, raw)
	default:
		writeJSONTest(w, 404, map[string]string{"error": "not found " + r.Method + " " + path})
	}
}

// serveApp answers an App's hostname: whoami, the files of a Volume, or
// the hash of a secret value.
func (f *fakeConsole) serveApp(w http.ResponseWriter, r *http.Request, host string) {
	for key, spec := range f.apps {
		if len(spec.Ports) == 0 || spec.Ports[0].Public != host {
			continue
		}
		if spec.Source.Git != nil && f.buildPoll > 0 {
			break
		}
		if f.appFail > 0 {
			f.appFail--
			http.Error(w, "no available server", http.StatusServiceUnavailable)
			return
		}
		project, _, _ := strings.Cut(key, "/")
		switch {
		case len(spec.Volumes) > 0 && spec.Volumes[0].Volume != "":
			files := f.volumes[project+"/"+spec.Volumes[0].Volume]
			content, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
			if !ok {
				http.Error(w, "404 Not Found", http.StatusNotFound)
				return
			}
			fmt.Fprint(w, content)
		case len(spec.Env) > 0 && spec.Env[0].ValueFrom != nil:
			ref := spec.Env[0].ValueFrom.SecretKeyRef
			value, ok := f.secrets[project+"/"+ref.Name][ref.Key]
			if !ok {
				http.Error(w, "the pod waits for its secret", http.StatusBadGateway)
				return
			}
			fmt.Fprintln(w, sha256Hex(value))
		default:
			fmt.Fprintf(w, "Hostname: %s-%d\nIP: 10.0.0.1\n", strings.ReplaceAll(key, "/", "-"), f.webGen)
		}
		return
	}
	app, _ := strings.CutSuffix(host, "."+f.domain)
	http.Error(w, "404 page not found "+app, http.StatusNotFound)
}

// ---- upgrades -------------------------------------------------------------------

func (f *fakeConsole) serveUpgrades(w http.ResponseWriter, r *http.Request, parts []string, raw []byte) {
	switch {
	case len(parts) == 3 && r.Method == "POST":
		var req upgradeRequest
		_ = json.Unmarshal(raw, &req)
		if req.Password != f.owner.Password {
			writeJSONTest(w, 400, map[string]string{"error": "Wrong password.", "field": "password"})
			return
		}
		if req.Cluster != "local" || (req.Component != "Kwerft" && req.Component != "Kubernetes") {
			writeJSONTest(w, 400, map[string]string{"error": "bad request"})
			return
		}
		if strings.Contains(req.Version, "-") && f.channel != "edge" {
			writeJSONTest(w, 409, map[string]any{"error": "The preflight failed: " + req.Version + " is on the edge channel; this console follows stable."})
			return
		}
		if f.preflightFail != "" {
			// As the console answers: the error first (Go sorts map keys).
			writeJSONTest(w, 409, map[string]any{"error": "The preflight failed: " + f.preflightFail,
				"preflight": map[string]any{"blocked": true, "checks": []upgradeCheck{{Check: "Target", OK: false, Message: f.preflightFail}}}})
			return
		}
		for _, u := range f.upgrades {
			if !u.view.Finished && u.view.Component == req.Component {
				writeJSONTest(w, 409, map[string]any{"error": "Upgrade " + u.view.Name + " is running.", "upgrade": u.view})
				return
			}
		}
		u := f.newUpgrade(req)
		f.upgrades[u.view.Name] = u
		writeJSONTest(w, 201, map[string]any{"upgrade": u.view})
	case len(parts) == 4 && r.Method == "GET":
		u, ok := f.upgrades[parts[3]]
		if !ok {
			writeJSONTest(w, 404, map[string]string{"error": "not found"})
			return
		}
		writeJSONTest(w, 200, u.view)
	case len(parts) == 5 && parts[4] == "log":
		writeJSONTest(w, 200, map[string]any{"log": f.upgradeLog, "truncated": false})
	default:
		writeJSONTest(w, 404, map[string]string{"error": "not found"})
	}
}

func (f *fakeConsole) newUpgrade(req upgradeRequest) *fakeUpgrade {
	slug := strings.NewReplacer(".", "-", "+", "-").Replace(req.Version)
	u := &fakeUpgrade{view: upgradeView{Name: strings.ToLower(req.Component) + "-" + strings.TrimPrefix(slug, "v") + "-x7k2p", Cluster: "local",
		Component: req.Component, Version: req.Version, Phase: "Pending", Steps: []upgradeStep{}, Nodes: []upgradeNode{}}, dropAt: -1}
	phase := func(p string) func(*fakeConsole, *upgradeView) {
		return func(_ *fakeConsole, v *upgradeView) { v.Phase = p }
	}
	finish := func(p, reason, msg string) func(*fakeConsole, *upgradeView) {
		return func(_ *fakeConsole, v *upgradeView) { v.Phase, v.Reason, v.Message, v.Finished = p, reason, msg, true }
	}
	if req.Component == "Kwerft" {
		from := f.version
		u.view.From = &upgradeFrom{Kwerft: from}
		u.script = []func(*fakeConsole, *upgradeView){
			phase("Preflight"), phase("Backup"),
			func(_ *fakeConsole, v *upgradeView) {
				v.Phase = "Running"
				v.Steps = []upgradeStep{{ID: "preflight", Label: "Preflight", State: "Done"}, {ID: "kwerft", Label: "Kwerft", State: "Running"}}
			},
			// The installer replaced the console: the stream breaks here.
			func(f *fakeConsole, v *upgradeView) {
				v.Steps[1].State, f.version = "Done", req.Version
				v.Steps = append(v.Steps, upgradeStep{ID: "handoff", Label: "Handoff", State: "Done"})
			},
			func(f *fakeConsole, v *upgradeView) {
				failed := u.fault == faultInstall || f.failUpgrade
				if !failed {
					v.Phase = "Verifying"
					return
				}
				v.Phase, f.version = "RollingBack", from
				if f.touchApps {
					f.webGen++
				}
			},
			func(f *fakeConsole, v *upgradeView) {
				switch {
				case u.fault == faultInstall:
					finish("RolledBack", "Kwerft", "Fault injected (e2e): the installer counts as failed. Rolled back to "+from+".")(f, v)
				case f.failUpgrade:
					finish("RolledBack", "Verify", "The console did not come up on "+req.Version+".")(f, v)
				default:
					finish("Succeeded", "", "")(f, v)
				}
			},
		}
		u.dropAt = 3
		return u
	}
	// Kubernetes: the control plane, then the workers, one by one.
	u.view.From = &upgradeFrom{Kubernetes: f.nodes[0].version}
	for _, n := range f.nodes {
		u.view.Nodes = append(u.view.Nodes, upgradeNode{Name: n.name, Version: n.version, State: "Waiting"})
	}
	u.script = append(u.script, phase("Preflight"), phase("Backup"), phase("Running"))
	for i, n := range f.nodes {
		states := []string{"Upgrading", "Done"}
		if n.role == "worker" {
			states = []string{"Draining", "Upgrading", "Done"}
		}
		for _, s := range states {
			u.script = append(u.script, func(f *fakeConsole, v *upgradeView) {
				v.Nodes[i].State = s
				if s == "Draining" {
					f.nodes[i].unschedulable = true
				}
				if s == "Upgrading" && i == 0 {
					f.appFail = 2 // the control plane restarts: a short gap
				}
				if s == "Done" {
					v.Nodes[i].Version, f.nodes[i].version, f.nodes[i].unschedulable = req.Version, req.Version, false
				}
			})
		}
	}
	u.script = append(u.script, phase("Verifying"), finish("Succeeded", "", ""))
	return u
}

// serveEvents is GET /api/v1/upgrades/{name}/events: each event advances
// the upgrade by one step.
func (f *fakeConsole) serveEvents(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	authed := false
	if c, err := r.Cookie("__Host-kwerft_session"); err == nil && f.sessions[c.Value] {
		authed = true
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	u := f.upgrades[parts[3]]
	down := f.down || f.version == ""
	f.mu.Unlock()
	switch {
	case down:
		http.Error(w, "no console", http.StatusBadGateway)
		return
	case !authed:
		writeJSONTest(w, 401, map[string]string{"error": "Sign in first."})
		return
	case u == nil:
		writeJSONTest(w, 404, map[string]string{"error": "not found"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	fmt.Fprint(w, ": ping\n\n")
	for {
		f.mu.Lock()
		data, _ := json.Marshal(u.view)
		finished := u.view.Finished
		drop := !u.dropped && u.pos == u.dropAt
		if drop {
			u.dropped = true
		}
		if !finished && !drop && u.pos < len(u.script) {
			u.script[u.pos](f, &u.view)
			u.pos++
		}
		phase := u.view.Phase
		f.mu.Unlock()
		fmt.Fprintf(w, "event: upgrade\ndata: %s\n\n", data)
		if drop {
			fl.Flush()
			return // the console restarted under the stream
		}
		if finished {
			fmt.Fprintf(w, "event: end\ndata: {\"phase\":%q}\n\n", phase)
			fl.Flush()
			return
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(time.Millisecond):
		}
	}
}

// ---- backups -------------------------------------------------------------------

func (f *fakeConsole) serveBackups(w http.ResponseWriter, r *http.Request, path string, raw []byte) {
	switch {
	case path == "/api/v1/settings/backups" && r.Method == "PUT":
		var t backupTarget
		_ = json.Unmarshal(raw, &t)
		if !strings.HasPrefix(t.Endpoint, "https://") || t.Bucket == "" || t.AccessKey == "" || t.SecretKey == "" {
			writeJSONTest(w, 400, map[string]string{"error": "Enter the bucket.", "field": "bucket"})
			return
		}
		out := map[string]any{"planCreated": f.target == nil}
		if f.recoveryKey == "" {
			var groups []string
			alphabet := "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
			b := make([]byte, 52)
			_, _ = rand.Read(b)
			for i := range b {
				b[i] = alphabet[int(b[i])%32]
			}
			for i := 0; i < 52; i += 4 {
				groups = append(groups, string(b[i:i+4]))
			}
			f.recoveryKey = strings.Join(groups, "-")
			out["recoveryKey"] = f.recoveryKey
		}
		f.target = &t
		writeJSONTest(w, 200, out)
	case path == "/api/v1/settings/backups":
		s := backupSettings{State: "NotConfigured"}
		if f.target != nil {
			s.Configured, s.Prefix, s.State = true, f.target.Prefix, "Pending"
			if f.targetPolls++; f.targetPolls > 1 {
				s.State = "Ready"
			}
		}
		writeJSONTest(w, 200, s)
	case path == "/api/v1/backups/plans/cluster/run" && r.Method == "POST":
		if f.target == nil {
			writeJSONTest(w, 404, map[string]string{"error": "Backup plan cluster not found."})
			return
		}
		name := "kwerft-cluster-" + time.Now().UTC().Format("20060102150405")
		f.backupList = append(f.backupList, backupView{Name: name, Plan: "cluster", Scope: "Cluster", Phase: "New"})
		writeJSONTest(w, 202, map[string]any{"backup": name})
	case path == "/api/v1/backups":
		for i := range f.backupList {
			b := &f.backupList[i]
			if b.Phase == "Completed" {
				continue
			}
			if f.backupPolls[b.Name]++; f.backupPolls[b.Name] > 2 {
				b.Phase, b.Items, b.Bytes = "Completed", 312, 4096
				// What Velero writes, under the prefix.
				p := f.target.Prefix + "/"
				for _, k := range []string{"velero/backups/" + b.Name + "/velero-backup.json", "velero/backups/" + b.Name + "/" + b.Name + ".tar.gz",
					"velero/kopia/e2e-restore/kopia.repository", "etcd/on-demand-snapshot"} {
					f.s3.put(p + k)
				}
			} else {
				b.Phase = "InProgress"
			}
		}
		writeJSONTest(w, 200, map[string]any{"velero": true, "backups": f.backupList})
	default:
		writeJSONTest(w, 404, map[string]string{"error": "not found " + path})
	}
}
