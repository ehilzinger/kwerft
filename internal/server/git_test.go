// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/git/gittest"
)

// Git connections, checks, "Build now" and webhooks against the test
// cluster (with the GitConnection reconciler running) and a fake Git host.

const (
	headSHA  = "4f2c1ab9d0e5c3b2a1908f7e6d5c4b3a29180716"
	otherSHA = "9a8b7c6d5e4f30211234567890abcdef12345678"
)

// gitHosts lets the test cluster's GitConnection reconciler reach the
// current test's fake host (whose TLS certificate only its client trusts).
var gitHosts = &swappableTransport{}

type swappableTransport struct {
	mu sync.Mutex
	rt http.RoundTripper
}

func (s *swappableTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.mu.Lock()
	rt := s.rt
	s.mu.Unlock()
	if rt == nil {
		return nil, errors.New("no fake Git host in this test")
	}
	return rt.RoundTrip(r)
}

func (s *swappableTransport) use(t *testing.T, fake *gittest.Server) {
	s.mu.Lock()
	s.rt = fake.Client().Transport
	s.mu.Unlock()
	t.Cleanup(func() { s.mu.Lock(); s.rt = nil; s.mu.Unlock() })
}

// lockedBuffer collects the console's log.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }

type gitEnv struct {
	*console
	fake *gittest.Server
	logs *lockedBuffer
	sys  client.Client
}

// newGitConsole starts a console whose own identity is the chart's
// kwerft-controller role (as in production), with a fake Git host.
func newGitConsole(t *testing.T, hooks ...func(*gitAPI)) *gitEnv {
	t.Helper()
	requireCluster(t)
	fake := gittest.New(t)
	fake.AddRepo("acme/api", "main", gittest.Commit{SHA: headSHA, Message: "Fix checkout\n\nDetails", Author: "Mara"})
	fake.SetBranch("acme/api", "release", gittest.Commit{SHA: otherSHA, Message: "Release", Author: "Ola"})
	fake.AddRepo("acme/secret", "main", gittest.Commit{SHA: headSHA, Message: "Private", Author: "Mara"}).Private = true
	gitHosts.use(t, fake)
	sys, err := client.New(cluster.console, client.Options{Scheme: cluster.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	c := newConsole(t, func(cfg *Config) {
		cfg.ConsoleDomain = "console.example.com"
		cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		cfg.System, cfg.SystemReader = sys, sys
		cfg.Git = fake.Factory()
		cfg.gitHook = func(g *gitAPI) {
			for _, h := range hooks {
				h(g)
			}
		}
	})
	return &gitEnv{console: c, fake: fake, logs: logs, sys: sys}
}

// connection creates a connection as the owner and waits until it is ready.
func (e *gitEnv) connection(t *testing.T, body map[string]any) (connectionJSON, string) {
	t.Helper()
	var out struct {
		Connection    connectionJSON `json:"connection"`
		WebhookSecret string         `json:"webhookSecret"`
	}
	if code := e.owner.do(t, "POST", "/api/v1/git/connections", body, &out); code != http.StatusCreated {
		t.Fatalf("create connection: %d %+v", code, out)
	}
	name := body["name"].(string)
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	var ready connectionJSON
	eventually(t, func() error {
		var list []connectionJSON
		e.viewer.do(t, "GET", "/api/v1/git/connections", nil, &list)
		for _, c := range list {
			if c.Name == name && c.Ready {
				ready = c
				return nil
			} else if c.Name == name {
				return fmt.Errorf("not ready: %s", c.Message)
			}
		}
		return fmt.Errorf("%s not listed", name)
	})
	return ready, out.WebhookSecret
}

func storedCredentials(t *testing.T, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)}, &sec); err != nil {
		t.Fatal(err)
	}
	return sec.Data
}

// noSecretsAnywhere checks that no response body seen, no audit entry and no
// log line contains any of the secrets.
func (e *gitEnv) noSecretsAnywhere(t *testing.T, bodies []string, secrets ...string) {
	t.Helper()
	entries, err := e.store.RecentAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	var audit strings.Builder
	for _, en := range entries {
		fmt.Fprintf(&audit, "%s %s %s %s\n", en.Actor, en.Action, en.Target, en.Detail)
	}
	logs := e.logs.String()
	for _, s := range secrets {
		if s == "" {
			continue
		}
		short := s
		if len(short) > 32 {
			short = short[len(s)/2-8 : len(s)/2+8] // a slice from the middle, e.g. of a PEM key
		}
		for _, b := range bodies {
			if strings.Contains(b, short) {
				t.Errorf("a response carries a secret: %s", b)
			}
		}
		if strings.Contains(audit.String(), short) {
			t.Errorf("the audit log carries a secret:\n%s", audit.String())
		}
		if strings.Contains(logs, short) {
			t.Errorf("the log carries a secret:\n%s", logs)
		}
	}
}

func TestGitConnectionsAreManagedByOwnersWithWriteOnlyCredentials(t *testing.T) {
	e := newGitConsole(t)
	ctx := context.Background()
	var bodies []string
	keep := func(r rawResponse) rawResponse { bodies = append(bodies, r.body); return r }

	input := map[string]any{"name": "gh", "provider": "github", "url": e.fake.URL, "auth": "token", "token": e.fake.Token}
	for _, s := range []*session{e.dev, e.viewer} {
		if r := keep(s.raw(t, "POST", "/api/v1/git/connections", input)); r.code != http.StatusForbidden {
			t.Errorf("create as non-admin: %d", r.code)
		}
	}
	// Validation, and a token the host rejects, are caught before anything is stored.
	for _, c := range []struct {
		change map[string]any
		field  string
	}{
		{map[string]any{"name": "Not_A_Name"}, "name"},
		{map[string]any{"url": "http://insecure.example.com"}, "url"},
		{map[string]any{"auth": "password"}, "auth"},
		{map[string]any{"token": ""}, "token"},
		{map[string]any{"token": "rejected-token"}, "token"},
		{map[string]any{"auth": "githubApp"}, "githubApp.appID"},
		{map[string]any{"auth": "sshKey", "token": "", "sshPrivateKey": "not a key"}, "sshPrivateKey"},
		{map[string]any{"projects": []string{"Bad Project"}}, "projects"},
	} {
		body := map[string]any{}
		for k, v := range input {
			body[k] = v
		}
		for k, v := range c.change {
			body[k] = v
		}
		var bad apiError
		if code := e.owner.do(t, "POST", "/api/v1/git/connections", body, &bad); code != http.StatusBadRequest || bad.Field != c.field {
			t.Errorf("%v: %d %+v", c.change, code, bad)
		}
	}
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "gh"}, &kwerftv1.GitConnection{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a refused create stored the connection: %v", err)
	}

	r := keep(e.owner.raw(t, "POST", "/api/v1/git/connections", input))
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var created struct {
		Connection    connectionJSON `json:"connection"`
		WebhookSecret string         `json:"webhookSecret"`
	}
	_ = json.Unmarshal([]byte(r.body), &created)
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "gh"}})
	})
	if len(created.WebhookSecret) < 40 || created.Connection.Name != "gh" || created.Connection.Auth != "token" {
		t.Errorf("created = %+v", created)
	}
	data := storedCredentials(t, "gh")
	if string(data[builds.KeyToken]) != e.fake.Token || string(data[builds.KeyWebhookSecret]) != created.WebhookSecret {
		t.Errorf("stored keys: %v", slices.Collect(func(yield func(string) bool) {
			for k := range data {
				if !yield(k) {
					return
				}
			}
		}))
	}

	// The reconciler verifies it; everyone reads the connection, nobody the token.
	var view connectionJSON
	eventually(t, func() error {
		var list []connectionJSON
		keep(e.viewer.raw(t, "GET", "/api/v1/git/connections", nil))
		e.viewer.do(t, "GET", "/api/v1/git/connections", nil, &list)
		// Connections are cluster-wide; other tests may have their own.
		for _, c := range list {
			if c.Name == "gh" && c.Ready {
				view = c
				return nil
			}
		}
		return fmt.Errorf("list = %+v", list)
	})
	if view.Account != "builder" || view.WebhookURL != "https://console.example.com/api/v1/hooks/git/gh" || !view.WebhookAutomatic {
		t.Errorf("view = %+v", view)
	}

	// Kubernetes holds the line too: owners and admins may patch this
	// Secret, and nothing more; developers and viewers not even that.
	for _, check := range []struct {
		role, verb, name string
		want             bool
	}{
		{"owner", "patch", "git-gh", true},
		{"admin", "patch", "git-gh", true},
		{"owner", "get", "git-gh", false},
		{"admin", "get", "git-gh", false},
		{"owner", "update", "git-gh", false},
		{"owner", "delete", "git-gh", false},
		{"owner", "patch", "registry-credentials", false},
		{"owner", "create", "", false},
		{"owner", "list", "", false},
		{"developer", "patch", "git-gh", false},
		{"viewer", "patch", "git-gh", false},
	} {
		if got := canName(t, check.role, builds.Namespace, check.verb, "", "secrets", "", check.name); got != check.want {
			t.Errorf("%s may %s secret %q in %s: %v, want %v", check.role, check.verb, check.name, builds.Namespace, got, check.want)
		}
	}

	// A new token; keeping the token while moving to another host is refused.
	if code := e.owner.do(t, "PUT", "/api/v1/git/connections/gh", map[string]any{"provider": "github", "url": e.fake.URL, "auth": "token",
		"token": e.fake.Token, "projects": []string{"shop"}}, &view); code != http.StatusOK || !slices.Equal(view.Projects, []string{"shop"}) {
		t.Errorf("update: %d %+v", code, view)
	}
	var bad apiError
	if code := e.owner.do(t, "PUT", "/api/v1/git/connections/gh", map[string]any{"provider": "github", "url": "https://github.example.org", "auth": "token"}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("moving the token to another host: %d %+v", code, bad)
	}
	if code := e.dev.do(t, "PUT", "/api/v1/git/connections/gh", map[string]any{"provider": "github", "url": e.fake.URL, "auth": "none"}, nil); code != http.StatusForbidden {
		t.Errorf("update as developer: %d", code)
	}

	// Rotation: shown once, stored, and only for owners and admins.
	var rotated struct {
		WebhookSecret string `json:"webhookSecret"`
	}
	r = keep(e.owner.raw(t, "POST", "/api/v1/git/connections/gh/webhook-secret", nil))
	_ = json.Unmarshal([]byte(r.body), &rotated)
	if r.code != http.StatusOK || rotated.WebhookSecret == "" || rotated.WebhookSecret == created.WebhookSecret ||
		string(storedCredentials(t, "gh")[builds.KeyWebhookSecret]) != rotated.WebhookSecret {
		t.Errorf("rotate: %d %+v", r.code, rotated)
	}
	if code := e.dev.do(t, "POST", "/api/v1/git/connections/gh/webhook-secret", nil, nil); code != http.StatusForbidden {
		t.Errorf("rotate as developer: %d", code)
	}

	// Public access: the token is removed, not left behind.
	if code := e.owner.do(t, "PUT", "/api/v1/git/connections/gh", map[string]any{"provider": "github", "url": e.fake.URL, "auth": "none"}, &view); code != http.StatusOK {
		t.Fatalf("to public: %d", code)
	}
	if data := storedCredentials(t, "gh"); len(data[builds.KeyToken]) != 0 || len(data[builds.KeyWebhookSecret]) == 0 {
		t.Errorf("after switching to public: token kept = %v", len(data[builds.KeyToken]) != 0)
	}

	// No endpoint, audit entry or log line ever shows a credential.
	for _, s := range []*session{e.owner, e.dev, e.viewer} {
		keep(s.raw(t, "GET", "/api/v1/git/connections", nil))
	}
	e.noSecretsAnywhere(t, bodies, e.fake.Token)
	// The webhook secret is in the create and rotate answers, by design,
	// and nowhere else.
	e.noSecretsAnywhere(t, nil, created.WebhookSecret, rotated.WebhookSecret)
	for _, s := range []*session{e.owner, e.dev, e.viewer} {
		if r := s.raw(t, "GET", "/api/v1/git/connections", nil); strings.Contains(r.body, rotated.WebhookSecret) {
			t.Errorf("the list shows the webhook secret: %s", r.body)
		}
	}
	var actions []string
	entries, _ := e.store.RecentAudit(ctx, 100)
	for _, en := range entries {
		actions = append(actions, en.Action)
	}
	for _, want := range []string{"git.connection_create", "git.credentials", "git.connection_update", "git.webhook_secret", "git.connection_create.denied"} {
		if !slices.Contains(actions, want) && want != "git.connection_create.denied" {
			t.Errorf("audit lacks %s: %v", want, actions)
		}
	}

	// In use: not deleted.
	app := gitAppObject("default", "api-uses-gh", e.fake.URL+"/acme/api", "gh")
	if err := cluster.admin.Create(ctx, app); err != nil {
		t.Fatal(err)
	}
	if code := e.owner.do(t, "DELETE", "/api/v1/git/connections/gh", nil, &bad); code != http.StatusConflict || !strings.Contains(bad.Error, "default/api-uses-gh") {
		t.Errorf("delete in use: %d %+v", code, bad)
	}
	_ = cluster.admin.Delete(ctx, app)
	eventually(t, func() error {
		return ignoreGone(cluster.admin.Get(ctx, client.ObjectKeyFromObject(app), &kwerftv1.App{}))
	})
	if code := e.dev.do(t, "DELETE", "/api/v1/git/connections/gh", nil, nil); code != http.StatusForbidden {
		t.Errorf("delete as developer: %d", code)
	}
	eventually(t, func() error {
		if code := e.owner.do(t, "DELETE", "/api/v1/git/connections/gh", nil, &bad); code != http.StatusNoContent {
			return fmt.Errorf("delete: %d %+v", code, bad)
		}
		return nil
	})
	eventually(t, func() error {
		return ignoreGone(cluster.admin.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: "git-gh"}, &corev1.Secret{}))
	})
}

func gitAppObject(ns, name, repository, connection string) *kwerftv1.App {
	return &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: repository, Branch: "main", Connection: connection}},
	}}
}

// deliver posts a webhook delivery as a Git host would: no cookies.
func (e *gitEnv) deliver(t *testing.T, connection string, hdr map[string]string, body string) (int, hookResult) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.owner.url+"/api/v1/hooks/git/"+connection, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var out hookResult
	_ = json.Unmarshal(raw, &out)
	return res.StatusCode, out
}

func githubHeaders(event, secret, body string) map[string]string {
	return map[string]string{"X-GitHub-Event": event, "X-GitHub-Delivery": "d-" + event, "X-Hub-Signature-256": "sha256=" + git.Sign([]byte(body), secret)}
}

func pushBody(fake *gittest.Server, branch, sha string) string {
	return `{"ref":"refs/heads/` + branch + `","after":"` + sha + `","repository":{"id":1,"full_name":"acme/api",
	  "clone_url":"` + fake.URL + `/acme/api.git","html_url":"` + fake.URL + `/acme/api"},
	  "head_commit":{"id":"` + sha + `","message":"Fix checkout\n\nDetails","author":{"name":"Mara"}},"sender":{"login":"mara-l"}}`
}

func listBuilds(t *testing.T, ns string) []kwerftv1.Build {
	t.Helper()
	var list kwerftv1.BuildList
	if err := cluster.admin.List(context.Background(), &list, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

func TestGitWebhooksCreateBuilds(t *testing.T) {
	e := newGitConsole(t)
	ctx := context.Background()
	e.project(t, "hookproj")
	e.project(t, "hookother")
	_, secret := e.connection(t, map[string]any{"name": "hooks", "provider": "github", "url": e.fake.URL, "auth": "token",
		"token": e.fake.Token, "projects": []string{"hookproj"}})

	repoURL := e.fake.URL + "/acme/api"
	noAuto := gitAppObject("hookproj", "noauto", "ssh://git@"+strings.TrimPrefix(e.fake.URL, "https://")+"/acme/api.git", "hooks")
	noAuto.Spec.Source.Git.AutoDeploy = new(bool)
	develop := gitAppObject("hookproj", "develop", repoURL, "hooks")
	develop.Spec.Source.Git.Branch = "develop"
	for _, app := range []*kwerftv1.App{
		gitAppObject("hookproj", "api", repoURL+".git", "hooks"),
		noAuto,
		develop,
		gitAppObject("hookproj", "other-conn", repoURL, ""),              // public, no connection
		gitAppObject("hookother", "api", repoURL, "hooks"),               // a project the connection does not serve
		gitAppObject("hookproj", "web", e.fake.URL+"/acme/web", "hooks"), // another repository
	} {
		if err := cluster.admin.Create(ctx, app); err != nil {
			t.Fatal(err)
		}
	}

	// Not signed, wrongly signed, wrong connection: nothing happens.
	body := pushBody(e.fake, "main", headSHA)
	for _, hdr := range []map[string]string{
		{"X-GitHub-Event": "push"},
		githubHeaders("push", "wrong-secret", body),
		{"X-GitHub-Event": "push", "X-Gitlab-Token": secret},
	} {
		if code, _ := e.deliver(t, "hooks", hdr, body); code != http.StatusUnauthorized {
			t.Errorf("%v: %d, want 401", hdr, code)
		}
	}
	if code, _ := e.deliver(t, "no-such-connection", githubHeaders("push", secret, body), body); code != http.StatusNotFound {
		t.Errorf("unknown connection: %d", code)
	}
	// A signature over another body does not carry over.
	if code, _ := e.deliver(t, "hooks", githubHeaders("push", secret, body), pushBody(e.fake, "main", otherSHA)); code != http.StatusUnauthorized {
		t.Errorf("tampered body: %d", code)
	}
	if n := len(listBuilds(t, "hookproj")); n != 0 {
		t.Fatalf("%d builds after rejected deliveries", n)
	}

	if code, res := e.deliver(t, "hooks", githubHeaders("ping", secret, `{"zen":"hi"}`), `{"zen":"hi"}`); code != http.StatusOK || res.Event != "ping" {
		t.Errorf("ping: %d %+v", code, res)
	}

	// A push to main builds api and noauto, nothing else.
	code, res := e.deliver(t, "hooks", githubHeaders("push", secret, body), body)
	if code != http.StatusAccepted || len(res.Builds) != 2 {
		t.Fatalf("push: %d %+v", code, res)
	}
	got := map[string]kwerftv1.Build{}
	for _, b := range listBuilds(t, "hookproj") {
		got[b.Spec.App] = b
	}
	api, ok := got["api"]
	if !ok || api.Spec.Trigger != "push" || !api.Spec.Deploy || api.Spec.Commit != headSHA || api.Spec.Branch != "main" ||
		api.Spec.Message != "Fix checkout" || api.Spec.Author != "Mara" || api.Spec.RequestedBy != "mara-l" || api.Spec.Source.Connection != "hooks" {
		t.Errorf("api build = %+v", api.Spec)
	}
	if b, ok := got["noauto"]; !ok || b.Spec.Deploy {
		t.Errorf("noauto build = %+v", b.Spec)
	}
	if len(got) != 2 || len(listBuilds(t, "hookother")) != 0 {
		t.Errorf("builds = %v", got)
	}

	// Redelivery: nothing new.
	code, res = e.deliver(t, "hooks", githubHeaders("push", secret, body), body)
	if code != http.StatusAccepted || len(res.Builds) != 0 || len(res.Existing) != 2 || len(listBuilds(t, "hookproj")) != 2 {
		t.Errorf("redelivery: %d %+v", code, res)
	}

	// Other branches, deletions and tags build nothing.
	for _, b := range []string{
		pushBody(e.fake, "feature", otherSHA),
		strings.Replace(pushBody(e.fake, "main", "0000000000000000000000000000000000000000"), `"after"`, `"deleted":true,"after"`, 1),
		strings.Replace(pushBody(e.fake, "main", otherSHA), "refs/heads/main", "refs/tags/v1.0.0", 1),
	} {
		if code, res := e.deliver(t, "hooks", githubHeaders("push", secret, b), b); code != http.StatusOK || res.Ignored == "" || len(res.Builds) != 0 {
			t.Errorf("ignored push: %d %+v", code, res)
		}
	}

	// Pull requests: same repository yes (no deploy), forks never.
	pr := func(action, headRepo string, headID int) string {
		return `{"action":"` + action + `","number":7,"pull_request":{"number":7,"title":"Add search","user":{"login":"sam"},
		  "head":{"ref":"feature","sha":"` + otherSHA + `","repo":{"id":` + fmt.Sprint(headID) + `,"full_name":"` + headRepo + `"}},
		  "base":{"ref":"main","repo":{"id":1,"full_name":"acme/api","clone_url":"` + e.fake.URL + `/acme/api.git"}}},"sender":{"login":"sam"}}`
	}
	fork := pr("opened", "mallory/api", 666)
	if code, res := e.deliver(t, "hooks", githubHeaders("pull_request", secret, fork), fork); code != http.StatusOK || !strings.Contains(res.Ignored, "fork") {
		t.Errorf("fork PR: %d %+v", code, res)
	}
	same := pr("opened", "acme/api", 1)
	code, res = e.deliver(t, "hooks", githubHeaders("pull_request", secret, same), same)
	if code != http.StatusAccepted || len(res.Builds) != 2 {
		t.Fatalf("PR: %d %+v", code, res)
	}
	var prBuild *kwerftv1.Build
	for _, b := range listBuilds(t, "hookproj") {
		if b.Spec.Trigger == "pull-request" && b.Spec.App == "api" {
			prBuild = &b
		}
	}
	if prBuild == nil || prBuild.Spec.Deploy || prBuild.Spec.PullRequest != 7 || prBuild.Spec.Commit != otherSHA || prBuild.Spec.Branch != "feature" {
		t.Errorf("PR build = %+v", prBuild)
	}
	if code, res := e.deliver(t, "hooks", githubHeaders("pull_request", secret, same), same); code != http.StatusAccepted || len(res.Builds) != 0 {
		t.Errorf("PR redelivery: %d %+v", code, res)
	}
	if n := len(listBuilds(t, "hookproj")); n != 4 {
		t.Errorf("%d builds, want 4", n)
	}

	// The connection shows the delivery; the audit log the builds and the
	// rejected deliveries, without the secret.
	var gc kwerftv1.GitConnection
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "hooks"}, &gc); err != nil || gc.Status.LastDelivery == nil {
		t.Errorf("lastDelivery = %v %v", gc.Status.LastDelivery, err)
	}
	entries, _ := e.store.RecentAudit(ctx, 100)
	builds, rejected := 0, 0
	for _, en := range entries {
		switch {
		case en.Action == "build.create" && en.Actor == "git:hooks":
			builds++
		case en.Action == "git.webhook_rejected":
			rejected++
		}
	}
	if builds != 4 || rejected < 3 {
		t.Errorf("audit: %d builds, %d rejected deliveries", builds, rejected)
	}
	e.noSecretsAnywhere(t, nil, secret, e.fake.Token)
}

func TestGitWebhookLimits(t *testing.T) {
	e := newGitConsole(t, func(g *gitAPI) {
		g.maxHookBody = 1 << 10
		g.hookIP = newLimiter(5, time.Minute, time.Now)
	})
	_, secret := e.connection(t, map[string]any{"name": "limits", "provider": "gitea", "url": e.fake.URL, "auth": "token", "token": e.fake.Token})
	big := `{"pad":"` + strings.Repeat("x", 2<<10) + `"}`
	if code, _ := e.deliver(t, "limits", map[string]string{"X-Gitea-Event": "push", "X-Gitea-Signature": git.Sign([]byte(big), secret)}, big); code != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d", code)
	}
	last := 0
	for range 6 {
		last, _ = e.deliver(t, "limits", map[string]string{"X-Gitea-Event": "push"}, `{}`)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("6th delivery: %d, want 429", last)
	}
}

func TestGitBuildNowAndCheck(t *testing.T) {
	e := newGitConsole(t)
	ctx := context.Background()
	e.project(t, "buildnow")
	e.connection(t, map[string]any{"name": "bn", "provider": "github", "url": e.fake.URL, "auth": "token", "token": e.fake.Token,
		"projects": []string{"buildnow"}})
	e.connection(t, map[string]any{"name": "bn-elsewhere", "provider": "gitlab", "url": e.fake.URL, "auth": "token", "token": e.fake.Token,
		"projects": []string{"someone-else"}})
	for _, app := range []*kwerftv1.App{
		gitAppObject("buildnow", "api", e.fake.URL+"/acme/api", "bn"),
		gitAppObject("buildnow", "excluded", e.fake.URL+"/acme/api", "bn-elsewhere"),
		{ObjectMeta: metav1.ObjectMeta{Namespace: "buildnow", Name: "image"}, Spec: kwerftv1.AppSpec{
			Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx:1"}}}},
	} {
		if err := cluster.admin.Create(ctx, app); err != nil {
			t.Fatal(err)
		}
	}

	// The branch head, as the owner, deployed.
	var b map[string]any
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/api/builds", map[string]any{}, &b); code != http.StatusCreated {
		t.Fatalf("build now: %d %v", code, b)
	}
	if b["commit"] != headSHA || b["trigger"] != "manual" || b["requestedBy"] != "owner@example.com" || b["deploy"] != true ||
		b["message"] != "Fix checkout" || b["branch"] != "main" || b["phase"] != "pending" || b["project"] != "buildnow" {
		t.Errorf("build = %v", b)
	}
	var stored kwerftv1.Build
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "buildnow", Name: b["name"].(string)}, &stored); err != nil || stored.Spec.Author != "Mara" {
		t.Errorf("stored = %+v %v", stored.Spec, err)
	}
	// A given commit; one the repository does not have.
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/api/builds", map[string]any{"commit": strings.ToUpper(otherSHA)}, &b); code != http.StatusCreated || b["commit"] != otherSHA || b["message"] != "Release" {
		t.Errorf("given commit: %d %v", code, b)
	}
	var bad apiError
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/api/builds", map[string]any{"commit": strings.Repeat("e", 40)}, &bad); code != http.StatusBadRequest || bad.Field != "commit" {
		t.Errorf("unknown commit: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/api/builds", map[string]any{"commit": "abc"}, &bad); code != http.StatusBadRequest || bad.Field != "commit" {
		t.Errorf("short commit: %d %+v", code, bad)
	}
	// Viewers may not build (Kubernetes says so); the connection's projects bind.
	if code := e.viewer.do(t, "POST", "/api/v1/projects/buildnow/apps/api/builds", map[string]any{}, nil); code != http.StatusForbidden {
		t.Errorf("viewer: %d", code)
	}
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/excluded/builds", map[string]any{}, &bad); code != http.StatusForbidden || !strings.Contains(bad.Error, "not available") {
		t.Errorf("connection of other projects: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "POST", "/api/v1/projects/buildnow/apps/image/builds", map[string]any{}, nil); code != http.StatusBadRequest {
		t.Errorf("image app: %d", code)
	}
	if n := len(listBuilds(t, "buildnow")); n != 2 {
		t.Errorf("%d builds, want 2", n)
	}

	// The deploy wizard's check.
	check := func(s *session, body map[string]any) (int, checkResult) {
		var out checkResult
		code := s.do(t, "POST", "/api/v1/git/check", body, &out)
		return code, out
	}
	if code, res := check(e.dev, map[string]any{"repository": e.fake.URL + "/acme/api", "project": "buildnow"}); code != http.StatusOK || !res.OK ||
		res.Connection != "bn" || res.DefaultBranch != "main" || res.Head == nil || res.Head.SHA != headSHA || res.Head.Message != "Fix checkout" ||
		res.Dockerfile == nil || !*res.Dockerfile {
		t.Errorf("check with a connection: %d %+v", code, res)
	}
	if _, res := check(e.dev, map[string]any{"repository": e.fake.URL + "/acme/api", "connection": "bn", "branch": "release", "path": "web"}); !res.OK ||
		res.Head.SHA != otherSHA || res.Dockerfile == nil || *res.Dockerfile || !strings.Contains(res.Message, "web/Dockerfile") {
		t.Errorf("check release/web: %+v", res)
	}
	if _, res := check(e.dev, map[string]any{"repository": e.fake.URL + "/acme/api", "connection": "bn-elsewhere", "project": "buildnow"}); res.OK {
		t.Errorf("connection of other projects: %+v", res)
	}
	if _, res := check(e.dev, map[string]any{"repository": e.fake.URL + "/acme/api", "branch": "nope", "connection": "bn"}); res.OK || !strings.Contains(res.Message, "nope") {
		t.Errorf("missing branch: %+v", res)
	}
	if _, res := check(e.dev, map[string]any{"repository": "https://user:" + e.fake.Token + "@github.com/acme/api"}); res.OK ||
		!strings.Contains(res.Message, "credentials") || strings.Contains(res.Message, e.fake.Token) {
		t.Errorf("credentials in the URL: %+v", res)
	}
	if _, res := check(e.dev, map[string]any{"repository": "not a url"}); res.OK {
		t.Errorf("bad URL: %+v", res)
	}
	if code, _ := check(e.viewer, map[string]any{"repository": e.fake.URL + "/acme/api"}); code != http.StatusForbidden {
		t.Errorf("viewer check: %d", code)
	}
	e.noSecretsAnywhere(t, nil, e.fake.Token)
}

func TestGitCheckWithoutConnection(t *testing.T) {
	e := newGitConsole(t)
	// No connection covers the fake host: anonymous ls-remote (sha only).
	var res checkResult
	if code := e.dev.do(t, "POST", "/api/v1/git/check", map[string]any{"repository": e.fake.URL + "/acme/api.git"}, &res); code != http.StatusOK || !res.OK ||
		res.Connection != "" || res.Head == nil || res.Head.SHA != headSHA || res.Dockerfile != nil {
		t.Errorf("public: %d %+v", code, res)
	}
	res = checkResult{}
	if e.dev.do(t, "POST", "/api/v1/git/check", map[string]any{"repository": e.fake.URL + "/acme/secret"}, &res); res.OK || !strings.Contains(res.Message, "private") {
		t.Errorf("private without a connection: %+v", res)
	}
}
