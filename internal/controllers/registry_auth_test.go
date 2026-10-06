package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"golang.org/x/crypto/bcrypt"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

// testZotBaseConfig is shaped like the chart's registry-config.yaml.
const testZotBaseConfig = `{
  "storage": {
    "rootDirectory": "/var/lib/registry",
    "gc": true,
    "retention": {
      "delay": "24h",
      "policies": [{"repositories": ["**"], "keepTags": [{"patterns": ["^keep-"]}, {"mostRecentlyPushedCount": 20, "pulledWithin": "2160h"}]}]
    }
  },
  "http": {"address": "0.0.0.0", "port": "5000"},
  "log": {"level": "info"}
}`

// zotConfig is the part of zot's config.json that decides access.
type zotConfig struct {
	Storage map[string]any `json:"storage"`
	HTTP    struct {
		Address string `json:"address"`
		Port    string `json:"port"`
		Auth    struct {
			HTPasswd struct {
				Path string `json:"path"`
			} `json:"htpasswd"`
		} `json:"auth"`
		AccessControl struct {
			Repositories map[string]struct {
				Policies []struct {
					Users   []string `json:"users"`
					Actions []string `json:"actions"`
				} `json:"policies"`
				DefaultPolicy   []string `json:"defaultPolicy"`
				AnonymousPolicy []string `json:"anonymousPolicy"`
			} `json:"repositories"`
			AdminPolicy struct {
				Users   []string `json:"users"`
				Actions []string `json:"actions"`
			} `json:"adminPolicy"`
		} `json:"accessControl"`
	} `json:"http"`
}

func parseZotConfig(t *testing.T, raw []byte) zotConfig {
	t.Helper()
	var c zotConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("config.json: %v\n%s", err, raw)
	}
	return c
}

// zotMatch is doublestar matching for the patterns Kwerft writes: "**",
// "<project>/**" and "<project>/**/<app>".
func zotMatch(pattern, repo string) bool {
	if pattern == "**" {
		return true
	}
	if prefix, ok := strings.CutSuffix(pattern, "/**"); ok {
		return strings.HasPrefix(repo, prefix+"/")
	}
	if prefix, name, ok := strings.Cut(pattern, "/**/"); ok {
		return repo == prefix+"/"+name || strings.HasPrefix(repo, prefix+"/") && strings.HasSuffix(repo, "/"+name)
	}
	return pattern == repo
}

// can decides like zot 2.1's AccessController.can: the longest pattern that
// matches the repository decides (its policies, then defaultPolicy for any
// authenticated user, anonymousPolicy for anonymous requests), then the
// admin policy. user "" is anonymous.
func (c zotConfig) can(user, action, repo string) bool {
	ac := c.HTTP.AccessControl
	longest := ""
	for p := range ac.Repositories {
		if zotMatch(p, repo) && len(p) > len(longest) {
			longest = p
		}
	}
	if g, ok := ac.Repositories[longest]; ok {
		for _, p := range g.Policies {
			if slices.Contains(p.Users, user) && slices.Contains(p.Actions, action) {
				return true
			}
		}
		if user != "" && slices.Contains(g.DefaultPolicy, action) {
			return true
		}
		if user == "" && slices.Contains(g.AnonymousPolicy, action) {
			return true
		}
	}
	return user != "" && slices.Contains(ac.AdminPolicy.Users, user) && slices.Contains(ac.AdminPolicy.Actions, action)
}

func TestRegistryConfigIsolatesProjects(t *testing.T) {
	raw, err := renderRegistryConfig([]byte(testZotBaseConfig), []string{"shop", "blog", "shop2"}, []string{"blog/web", "old/api", "shop/ui"})
	if err != nil {
		t.Fatal(err)
	}
	c := parseZotConfig(t, raw)
	if c.HTTP.Address != "0.0.0.0" || c.HTTP.Port != "5000" || c.HTTP.Auth.HTPasswd.Path != "/etc/zot/htpasswd" {
		t.Errorf("http = %+v", c.HTTP)
	}
	// The base config is kept as it was, numbers included.
	if !bytes.Contains(raw, []byte(`"mostRecentlyPushedCount": 20`)) || !bytes.Contains(raw, []byte(`"rootDirectory": "/var/lib/registry"`)) {
		t.Errorf("storage changed:\n%s", raw)
	}
	shop, blog, shop2 := builds.RegistryUser("shop"), builds.RegistryUser("blog"), builds.RegistryUser("shop2")
	for _, tc := range []struct {
		user, action, repo string
		want               bool
	}{
		// A project's user pushes and re-tags in its own repositories ...
		{shop, "create", "shop/api", true},
		{shop, "update", "shop/api", true},
		{shop, "read", "shop/api", true},
		{shop, "create", "shop/deep/name", true},
		// ... and nowhere else: not another project's, not one whose name
		// starts like its own, not outside any project.
		{shop, "create", "blog/web", false},
		{shop, "update", "blog/web", false},
		{shop, "create", "blog/api", false},
		{shop, "create", "shop2/api", false},
		{shop2, "create", "shop/api", false},
		{shop, "create", "kwerft/api", false},
		{shop, "create", "shopapi", false},
		{blog, "create", "shop/api", false},
		{blog, "create", "blog/web", true},
		// Nobody deletes but Kwerft.
		{shop, "delete", "shop/api", false},
		{"", "delete", "shop/api", false},
		{RegistryAdminUser, "delete", "shop/api", true},
		{RegistryAdminUser, "create", "blog/web", true},
		{RegistryAdminUser, "update", "old/api", true},
		// Anonymous: read anything (the nodes pull without a credential),
		// push nothing, except to a running legacy build's own repository.
		{"", "read", "shop/api", true},
		{"", "read", "nobody/x", true},
		{"", "create", "shop/api", false},
		{"", "update", "shop/api", false},
		{"", "create", "nobody/x", false},
		{"", "create", "blog/web", true},
		{"", "update", "old/api", true},
		{"", "create", "blog/other", false},
		// ... also when the App's name is shorter than "**" (the legacy pattern
		// must still be the longest match).
		{"", "create", "shop/ui", true},
		{shop, "update", "shop/ui", true},
		{blog, "create", "shop/ui", false},
		// An unknown user gets nothing: a valid credential of a deleted
		// project is useless (and its htpasswd line is gone anyway).
		{builds.RegistryUser("gone"), "create", "gone/api", false},
	} {
		if got := c.can(tc.user, tc.action, tc.repo); got != tc.want {
			t.Errorf("%q %s %s: %v, want %v", tc.user, tc.action, tc.repo, got, tc.want)
		}
	}

	// No projects: Kwerft and anonymous reads only.
	raw, err = renderRegistryConfig([]byte(testZotBaseConfig), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	c = parseZotConfig(t, raw)
	if len(c.HTTP.AccessControl.Repositories) != 1 || c.can(shop, "create", "shop/api") || !c.can("", "read", "shop/api") {
		t.Errorf("repositories = %+v", c.HTTP.AccessControl.Repositories)
	}
	// Names that cannot be patterns are left out rather than widening one.
	raw, _ = renderRegistryConfig([]byte(testZotBaseConfig), []string{"a*b", "ok"}, []string{"ok/../x", "ok/y/z"})
	c = parseZotConfig(t, raw)
	if len(c.HTTP.AccessControl.Repositories) != 2 {
		t.Errorf("repositories = %+v", c.HTTP.AccessControl.Repositories)
	}
	if _, err := renderRegistryConfig([]byte("not json"), nil, nil); err == nil {
		t.Error("a broken base config was accepted")
	}
	// Deterministic: the same input renders the same bytes (zot reloads on
	// every change).
	a, _ := renderRegistryConfig([]byte(testZotBaseConfig), []string{"shop", "blog"}, nil)
	b, _ := renderRegistryConfig([]byte(testZotBaseConfig), []string{"shop", "blog"}, nil)
	if !bytes.Equal(a, b) {
		t.Error("rendering is not deterministic")
	}
}

func TestRegistryCredential(t *testing.T) {
	c, err := newRegistryCredential(builds.RegistryUser("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.password) < 40 || !strings.HasPrefix(c.line, "project-shop:$2") {
		t.Errorf("credential = %+v", c)
	}
	data := map[string][]byte{
		builds.KeyRegistryUsername: []byte(c.user), builds.KeyRegistryPassword: []byte(c.password), builds.KeyRegistryHTPasswd: []byte(c.line),
	}
	if got, ok := credentialFrom(data, c.user); !ok || got != c {
		t.Errorf("read back %+v %v", got, ok)
	}
	if _, ok := credentialFrom(data, builds.RegistryUser("blog")); ok {
		t.Error("another project's user accepted")
	}
	tampered := map[string][]byte{builds.KeyRegistryUsername: []byte(c.user), builds.KeyRegistryPassword: []byte("guess"), builds.KeyRegistryHTPasswd: []byte(c.line)}
	if _, ok := credentialFrom(tampered, c.user); ok {
		t.Error("a password that does not match the hash accepted")
	}
	if _, ok := credentialFrom(nil, c.user); ok {
		t.Error("an empty Secret accepted")
	}
	other, _ := newRegistryCredential(c.user)
	if other.password == c.password {
		t.Error("two credentials share a password")
	}

	var cfg struct {
		Auths map[string]struct{ Username, Password, Auth string } `json:"auths"`
	}
	if err := json.Unmarshal(dockerConfig(c), &cfg); err != nil {
		t.Fatal(err)
	}
	a, ok := cfg.Auths["registry.kwerft.internal:5000"]
	auth, _ := base64.StdEncoding.DecodeString(a.Auth)
	if !ok || len(cfg.Auths) != 1 || a.Username != c.user || a.Password != c.password || string(auth) != c.user+":"+c.password {
		t.Errorf("docker config = %+v", cfg)
	}

	if got := string(renderHTPasswd([]string{"b:2", "a:1"})); got != "a:1\nb:2\n" {
		t.Errorf("htpasswd = %q", got)
	}
}

func TestJobRepository(t *testing.T) {
	job := func(project, image string) *batchv1.Job {
		j := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{LabelProject: project}}}
		j.Spec.Template.Spec.Containers = []corev1.Container{{Name: "build", Env: []corev1.EnvVar{{Name: "KWERFT_IMAGE", Value: image}}}}
		return j
	}
	for _, tc := range []struct{ project, image, want string }{
		{"shop", builds.ImageRef("shop", "api", sha(1)), "shop/api"},
		{"shop", builds.ImageRepository("shop", "api"), "shop/api"},
		{"shop", builds.ImageRef("blog", "web", sha(1)), ""}, // not its own project
		{"shop", "ghcr.io/acme/api:1", ""},
		{"shop", builds.RegistryHost + "/shop", ""},
	} {
		if got := jobRepository(job(tc.project, tc.image)); got != tc.want {
			t.Errorf("%s %s: %q, want %q", tc.project, tc.image, got, tc.want)
		}
	}
}

func TestRegistryKeeperAuthenticates(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	reg := &fakeRegistry{
		manifests: map[string][]byte{d1: []byte(`{"m":1}`)},
		tags:      map[string]string{"aaa": d1},
		user:      RegistryAdminUser, password: "s3cret",
	}
	srv := newRegistryServer(t, reg)
	history := []kwerftv1.AppRevision{{Number: 1, Image: builds.ImageRepository("shop", "api") + ":aaa@" + d1}}
	anonymous := &RegistryKeeper{URL: srv.URL, HTTP: srv.Client()}
	if err := anonymous.Keep(context.Background(), "shop", "api", history); err == nil {
		t.Error("an anonymous keeper tagged an image")
	}
	k := &RegistryKeeper{URL: srv.URL, HTTP: srv.Client(), Credentials: func(context.Context) (string, string, error) {
		return RegistryAdminUser, "s3cret", nil
	}}
	if err := k.Keep(context.Background(), "shop", "api", history); err != nil {
		t.Fatal(err)
	}
	if reg.tags[keepTag(d1)] != d1 {
		t.Errorf("tags = %v", reg.tags)
	}
}

func TestRegistryProbe(t *testing.T) {
	status := http.StatusUnauthorized
	var gotUser, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, _, _ = r.BasicAuth()
		gotPath = r.URL.Path
		w.WriteHeader(status)
	}))
	defer srv.Close()
	probe := RegistryProbe(srv.URL, srv.Client())
	for _, tc := range []struct {
		status int
		ok     bool
		err    bool
	}{
		{http.StatusUnauthorized, false, false}, // zot does not know the user yet
		{http.StatusForbidden, false, false},    // ... or not the project's access
		{http.StatusNotFound, true, false},      // both: no such repository
		{http.StatusOK, true, false},
		{http.StatusInternalServerError, false, true},
	} {
		status = tc.status
		ok, err := probe(context.Background(), "shop", "project-shop", "pw")
		if ok != tc.ok || (err != nil) != tc.err {
			t.Errorf("%d: %v %v", tc.status, ok, err)
		}
	}
	if gotUser != "project-shop" || gotPath != "/v2/shop/kwerft-probe/tags/list" {
		t.Errorf("probe sent %q to %q", gotUser, gotPath)
	}

	c, _ := newRegistryCredential("project-shop")
	sec := &corev1.Secret{Data: map[string][]byte{builds.KeyRegistryHTPasswd: []byte(c.line), corev1.DockerConfigJsonKey: dockerConfig(c)}}
	if RegistryCredentialActive(sec) {
		t.Error("active without the annotation")
	}
	sec.Annotations = map[string]string{AnnotationRegistryActive: registryFingerprint(c.line)}
	if !RegistryCredentialActive(sec) {
		t.Error("not active with the annotation")
	}
	other, _ := newRegistryCredential("project-shop")
	sec.Data[builds.KeyRegistryHTPasswd] = []byte(other.line)
	if RegistryCredentialActive(sec) {
		t.Error("a replaced credential counts as active")
	}
}

// ---- envtest -----------------------------------------------------------------------

// testRegistryRefuses holds the projects whose credential the suite's
// registry probe does not accept (yet).
var testRegistryRefuses sync.Map

func testRegistryProbe(_ context.Context, project, _, _ string) (bool, error) {
	_, refused := testRegistryRefuses.Load(project)
	return !refused, nil
}

func TestBuildWaitsUntilRegistryAcceptsCredential(t *testing.T) {
	requireEnvtest(t)
	testRegistryRefuses.Store("bwait", true)
	t.Cleanup(func() { testRegistryRefuses.Delete("bwait") })
	app := gitProject(t, "bwait", "api", gitAppSpec("https://github.com/acme/api.git", ""))
	b := newBuild(t, app, 1, "push", true, nil)
	b = waitForBuild(t, b, func(b *kwerftv1.Build) error {
		if b.Status.Message != "Waiting for the project's registry credentials" {
			return fmt.Errorf("message %q", b.Status.Message)
		}
		return nil
	})
	var sec corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: builds.Namespace, Name: builds.RegistrySecret("bwait")}, &sec); err != nil {
		t.Fatal(err)
	}
	if b.Status.Job != "" || RegistryCredentialActive(&sec) {
		t.Fatalf("started with a credential the registry refuses: job %q", b.Status.Job)
	}
	// The registry takes the credential: the build starts.
	testRegistryRefuses.Delete("bwait")
	waitForBuild(t, b, buildStarted)
}

// zotAuth reads what zot runs with.
func zotAuth(t *testing.T) (zotConfig, string) {
	t.Helper()
	var sec corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: builds.RegistryNamespace, Name: RegistryAuthSecret}, &sec); err != nil {
		t.Fatal(err)
	}
	return parseZotConfig(t, sec.Data[registryConfigKey]), string(sec.Data[registryHTPasswdKey])
}

// htpasswdAccepts checks a password against the htpasswd line of user.
func htpasswdAccepts(htpasswd, user, password string) bool {
	for line := range strings.Lines(htpasswd) {
		if hash, ok := strings.CutPrefix(strings.TrimSpace(line), user+":"); ok {
			return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
		}
	}
	return false
}

func TestRegistryCredentialsPerProject(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "rega")
	b := createProject(t, "regb", kwerftv1.ProjectSpec{})

	creds := map[string]corev1.Secret{}
	eventually(t, func() error {
		for _, p := range []string{"rega", "regb"} {
			var sec corev1.Secret
			if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.RegistrySecret(p)}, &sec); err != nil {
				return err
			}
			creds[p] = sec
		}
		_, htpasswd := zotAuth(t)
		for p, sec := range creds {
			if !htpasswdAccepts(htpasswd, builds.RegistryUser(p), string(sec.Data[builds.KeyRegistryPassword])) {
				return fmt.Errorf("zot does not know %s's credential yet", p)
			}
		}
		return nil
	})
	a := creds["rega"]
	if a.Type != corev1.SecretTypeDockerConfigJson || a.Labels[LabelRegistryCredential] != "rega" || len(a.OwnerReferences) != 1 ||
		a.OwnerReferences[0].Kind != "Project" || a.OwnerReferences[0].Name != "rega" {
		t.Errorf("secret = %+v", a.ObjectMeta)
	}
	if string(a.Data[builds.KeyRegistryUsername]) != "project-rega" || !bytes.Contains(a.Data[corev1.DockerConfigJsonKey], a.Data[builds.KeyRegistryPassword]) {
		t.Errorf("secret data keys = %v", a.Data)
	}
	if bytes.Equal(a.Data[builds.KeyRegistryPassword], creds["regb"].Data[builds.KeyRegistryPassword]) {
		t.Error("two projects share a password")
	}
	cfg, htpasswd := zotAuth(t)
	if !cfg.can("project-rega", "create", "rega/api") || cfg.can("project-rega", "create", "regb/api") || cfg.can("project-regb", "update", "rega/api") {
		t.Errorf("access control = %+v", cfg.HTTP.AccessControl)
	}
	// Kwerft's own credential, for keep tags.
	var admin corev1.Secret
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: RegistryAdminSecret}, &admin); err != nil {
		t.Fatal(err)
	}
	user, password, err := RegistryAdminCredentials(k8s)(ctx)
	if err != nil || user != RegistryAdminUser || !htpasswdAccepts(htpasswd, user, password) || !cfg.can(user, "delete", "rega/api") {
		t.Errorf("admin credential %q: %v", user, err)
	}

	// Stable: another pass (the base config changes) keeps every password.
	var cm corev1.ConfigMap
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: builds.RegistryService}, &cm); err != nil {
		t.Fatal(err)
	}
	cm.Data[registryConfigKey] = strings.Replace(testZotBaseConfig, `"level": "info"`, `"level": "debug"`, 1)
	if err := k8s.Update(ctx, &cm); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = k8s.Get(ctx, client.ObjectKeyFromObject(&cm), &cm)
		cm.Data[registryConfigKey] = testZotBaseConfig
		_ = k8s.Update(ctx, &cm)
	})
	eventually(t, func() error {
		var sec corev1.Secret
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: RegistryAuthSecret}, &sec); err != nil {
			return err
		}
		if !bytes.Contains(sec.Data[registryConfigKey], []byte(`"debug"`)) {
			return fmt.Errorf("base config not taken over")
		}
		return nil
	})
	var again corev1.Secret
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&a), &again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Data[builds.KeyRegistryPassword], a.Data[builds.KeyRegistryPassword]) {
		t.Error("the password changed")
	}

	// A broken credential is replaced (and zot told).
	again.Data[builds.KeyRegistryPassword] = []byte("tampered")
	if err := k8s.Update(ctx, &again); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		var sec corev1.Secret
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(&a), &sec); err != nil {
			return err
		}
		pw := string(sec.Data[builds.KeyRegistryPassword])
		if _, h := zotAuth(t); pw == "tampered" || !htpasswdAccepts(h, "project-rega", pw) {
			return fmt.Errorf("credential not repaired")
		}
		return nil
	})

	// The project goes: so do its credential and its user (envtest has no
	// garbage collector; the reconciler removes it itself).
	if err := k8s.Delete(ctx, b); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.RegistrySecret("regb")}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("credential of regb: %v", err)
		}
		cfg, htpasswd := zotAuth(t)
		if strings.Contains(htpasswd, "project-regb:") {
			return fmt.Errorf("regb still in htpasswd")
		}
		if _, ok := cfg.HTTP.AccessControl.Repositories["regb/**"]; ok {
			return fmt.Errorf("regb still in the access control")
		}
		return nil
	})
}

func TestRegistryLetsLegacyBuildsFinish(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	// A build Job from before credentials (no kwerft.dev/registry-auth),
	// of a Build the Build reconciler leaves alone.
	ensureNamespace(t, "gitstatus")
	build := &kwerftv1.Build{ObjectMeta: metav1.ObjectMeta{Namespace: "gitstatus", Name: "legacy-1"}, Spec: kwerftv1.BuildSpec{
		App: "legacy", Commit: sha(1), Trigger: "manual",
		Source: kwerftv1.BuildSource{Repository: "https://github.com/acme/legacy.git", Builder: "dockerfile"},
	}}
	if err := k8s.Create(ctx, build); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, build) })
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: "gitstatus-legacy-1",
			Labels: map[string]string{builds.LabelBuild: "legacy-1", LabelProject: "gitstatus"}},
		Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{Name: "build", Image: DefaultBuildKitImage,
				Env: []corev1.EnvVar{{Name: "KWERFT_IMAGE", Value: builds.ImageRef("gitstatus", "legacy", sha(1))}}}},
		}}},
	}
	if err := k8s.Create(ctx, job); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if cfg, _ := zotAuth(t); !cfg.can("", "create", "gitstatus/legacy") || cfg.can("", "create", "gitstatus/other") {
			return fmt.Errorf("no anonymous push for the running legacy build")
		}
		return nil
	})
	// Its end ends the exception.
	if err := k8s.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if cfg, _ := zotAuth(t); cfg.can("", "create", "gitstatus/legacy") {
			return fmt.Errorf("anonymous push still allowed")
		}
		return nil
	})
}
