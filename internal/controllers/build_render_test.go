package controllers

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

func TestBuildScriptsParse(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	for name, script := range map[string]string{"clone": cloneScript, "prepare": prepareScript, "build": buildScript} {
		if out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s script: %v\n%s", name, err, out)
		}
	}
}

func TestParseTermination(t *testing.T) {
	cases := []struct{ in, digest, msg string }{
		{"digest=sha256:abc\n", "sha256:abc", ""},
		{"error=Dockerfile not found: Dockerfile", "", "Dockerfile not found: Dockerfile"},
		{"#5 [2/3] RUN go build\n#5 0.31 main.go:3: undefined: x\n#5 ERROR: process \"/bin/sh -c go build\" did not complete successfully: exit code: 1\n------\n", "",
			"#5 ERROR: process \"/bin/sh -c go build\" did not complete successfully: exit code: 1"},
		{"Cloning x\nfatal: Authentication failed for 'https://github.com/acme/api.git/'\n", "", "fatal: Authentication failed for 'https://github.com/acme/api.git/'"},
		{"\x1b[31msomething broke\x1b[0m\n\n", "", "something broke"},
		{"", "", ""},
	}
	for _, c := range cases {
		got := parseTermination(c.in)
		if got.digest != c.digest || got.message != c.msg {
			t.Errorf("parseTermination(%q) = %+v", c.in, got)
		}
	}
	if long := oneLine(strings.Repeat("x", 500)); len(long) != 300 || !strings.HasSuffix(long, "...") {
		t.Errorf("oneLine = %d chars", len(long))
	}
}

func TestBuildHelpers(t *testing.T) {
	if got := humanDuration(30 * time.Minute); got != "30m" {
		t.Errorf("humanDuration = %s", got)
	}
	if got := humanDuration(time.Hour); got != "1h" {
		t.Errorf("humanDuration = %s", got)
	}
	if got := humanDuration(90 * time.Second); got != "1m30s" {
		t.Errorf("humanDuration = %s", got)
	}
	long := buildJobName(strings.Repeat("p", 63), "storefront-api-4f2c1ab-x7k2p")
	if len(long) > 63 || long == buildJobName(strings.Repeat("p", 63), "storefront-api-4f2c1ab-x7k2q") {
		t.Errorf("buildJobName = %s (%d)", long, len(long))
	}
	if got := buildJobName("shop", "api-4f2c1ab-x7k2p"); got != "shop-api-4f2c1ab-x7k2p" {
		t.Errorf("buildJobName = %s", got)
	}
	for in, want := range map[string]string{"/": "", "": "", "/services/api/": "services/api", "../../etc": "etc", "a/../../b": "b", "Dockerfile": "Dockerfile"} {
		if got := cleanRepoPath(in); got != want {
			t.Errorf("cleanRepoPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://github.com/acme/api.git":       "https",
		"https://user:pw@github.com/acme/x.git": "",
		"git@github.com:acme/api.git":           "ssh",
		"ssh://git@gitlab.example.com/acme/api": "ssh",
		"file:///etc":                           "",
		"ext::sh -c touch% /tmp/pwned":          "",
		"git://github.com/acme/api.git":         "",
		"-uhttps://x":                           "",
		"git@github.com:-oProxyCommand=evil":    "",
		"http://github.com/acme/api.git":        "",
	} {
		if got := repoTransport(in); got != want {
			t.Errorf("repoTransport(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckConnection(t *testing.T) {
	gh := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "gh"}, Spec: kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.GitHub, URL: "https://github.com", Auth: kwerftv1.GitAuthToken, Owner: "Acme", Projects: []string{"shop"}}}
	if err := checkConnection(gh, "shop", "https://github.com/acme/api.git", "https"); err != nil {
		t.Error(err)
	}
	for _, c := range []struct{ project, repo, transport, reason string }{
		{"blog", "https://github.com/acme/api.git", "https", "ConnectionNotAllowed"},
		{"shop", "https://github.com.evil.io/acme/api.git", "https", "ConnectionMismatch"},
		{"shop", "https://github.com/other/api.git", "https", "ConnectionMismatch"},
		{"shop", "git@github.com:acme/api.git", "ssh", "ConnectionMismatch"},
	} {
		if err := checkConnection(gh, c.project, c.repo, c.transport); reasonOf(err) != c.reason {
			t.Errorf("%+v: %v", c, err)
		}
	}
	key := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "key"}, Spec: kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.Generic, URL: "https://git.example.com", Auth: kwerftv1.GitAuthSSHKey}}
	if err := checkConnection(key, "shop", "git@git.example.com:acme/api.git", "ssh"); err != nil {
		t.Error(err)
	}
	if err := checkConnection(key, "shop", "https://git.example.com/acme/api.git", "https"); reasonOf(err) != "ConnectionMismatch" {
		t.Error(err)
	}
}

func testRun(builder, auth string) *buildRun {
	b := &kwerftv1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "api-4f2c1ab-x7k2p", Namespace: "shop"},
		Spec: kwerftv1.BuildSpec{App: "api", Commit: sha(7), Trigger: "push",
			Source: kwerftv1.BuildSource{Repository: "git@github.com:acme/api.git", Builder: builder, Path: "services/api"}},
	}
	return &buildRun{
		build: b, project: "shop", jobName: buildJobName("shop", b.Name),
		image: builds.ImageRef("shop", "api", b.Spec.Commit), cache: builds.CacheRef("shop", "api"),
		buildkitImage: DefaultBuildKitImage, railpackImage: DefaultRailpackImage, registryIP: builds.RegistryClusterIP,
		timeout: 20 * time.Minute, contextDir: "services/api", dockerfile: "Dockerfile",
		auth: auth, credentials: "git-deploykey", registrySecret: builds.RegistrySecret("shop"),
	}
}

// renderJob turns the apply configuration into a Job.
func renderJob(t *testing.T, r *buildRun) *batchv1.Job {
	t.Helper()
	raw, err := json.Marshal(r.job())
	if err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	return &job
}

func TestRenderRailpackSSHJob(t *testing.T) {
	job := renderJob(t, testRun("railpack", "ssh"))
	if *job.Spec.ActiveDeadlineSeconds != 1200 {
		t.Errorf("deadline = %d", *job.Spec.ActiveDeadlineSeconds)
	}
	pod := job.Spec.Template.Spec
	if len(pod.InitContainers) != 2 || pod.InitContainers[1].Name != builds.ContainerPrepare {
		t.Fatalf("init containers = %+v", pod.InitContainers)
	}
	prep := pod.InitContainers[1]
	if prep.Image != DefaultRailpackImage || len(prep.Command) != 5 || prep.Command[2] != prepareScript || prep.Command[4] != "/workspace/src/services/api" {
		t.Errorf("prepare = %s %v", prep.Image, prep.Command)
	}
	if job.Spec.Template.Annotations["kubectl.kubernetes.io/default-container"] != builds.ContainerBuild {
		t.Errorf("pod annotations = %v", job.Spec.Template.Annotations)
	}
	for _, c := range append(pod.InitContainers, pod.Containers...) {
		if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() || c.Resources.Requests.Memory().IsZero() {
			t.Errorf("%s: resources %+v", c.Name, c.Resources)
		}
		if c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
			t.Errorf("%s: termination message policy %s", c.Name, c.TerminationMessagePolicy)
		}
	}
	for _, v := range pod.Volumes {
		if v.EmptyDir != nil && v.EmptyDir.SizeLimit == nil {
			t.Errorf("volume %s has no size limit", v.Name)
		}
		if v.Name == "credentials" {
			if keys := []string{v.Secret.Items[0].Key, v.Secret.Items[1].Key}; len(v.Secret.Items) != 2 ||
				keys[0] != builds.KeySSHPrivateKey || keys[1] != builds.KeyKnownHosts {
				t.Errorf("credential items = %+v", v.Secret.Items)
			}
		}
	}
	var env []string
	for _, e := range pod.Containers[0].Env {
		env = append(env, e.Name+"="+e.Value)
	}
	for _, want := range []string{"KWERFT_BUILDER=railpack", "KWERFT_RAILPACK_FRONTEND=" + DefaultRailpackImage,
		"KWERFT_CACHE_KEY=shop/api", "KWERFT_REGISTRY=registry.kwerft.internal:5000", "KWERFT_CONTEXT=services/api"} {
		if !strings.Contains(strings.Join(env, "\n"), want) {
			t.Errorf("build env lacks %s: %v", want, env)
		}
	}
	if job := renderJob(t, testRun("dockerfile", "none")); len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Error("a Dockerfile build got a prepare step")
	} else {
		for _, v := range job.Spec.Template.Spec.Volumes {
			if v.Name == "credentials" {
				t.Error("a public clone mounts credentials")
			}
		}
	}
}

func TestFailureMessage(t *testing.T) {
	cond := &batchv1.JobCondition{Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}
	if got := failureMessage(nil, cond); got != cond.Message {
		t.Errorf("no pod: %q", got)
	}
	term := func(name string, code int32, reason, msg string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason, Message: msg}}}
	}
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{term(builds.ContainerClone, 1, "Error", "error=commit abc not found in x")},
	}}
	if got := failureMessage(pod, cond); got != "Clone failed: commit abc not found in x" {
		t.Errorf("clone: %q", got)
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{term(builds.ContainerClone, 0, "Completed", ""), term(builds.ContainerPrepare, 1, "Error", "")}
	if got := failureMessage(pod, cond); got != "Railpack: Preparing the Railpack plan failed (exit code 1)" {
		t.Errorf("prepare: %q", got)
	}
	pod.Status.InitContainerStatuses = pod.Status.InitContainerStatuses[:1]
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{term(builds.ContainerBuild, 137, "OOMKilled", "")}
	if got := failureMessage(pod, cond); got != "Building ran out of memory" {
		t.Errorf("oom: %q", got)
	}
}

func TestNewerBuildAndDeployImage(t *testing.T) {
	at := func(min int) *metav1.Time {
		tm := metav1.NewTime(time.Date(2026, 10, 4, 12, min, 0, 0, time.UTC))
		return &tm
	}
	b := func(n int64, done *metav1.Time) *kwerftv1.Build {
		return &kwerftv1.Build{Status: kwerftv1.BuildStatus{Number: n, CompletionTime: done, Image: "r/x:1"}}
	}
	if !newerBuild(b(1, at(5)), nil) || newerBuild(b(3, at(1)), b(2, at(5))) || !newerBuild(b(2, at(5)), b(3, at(1))) ||
		!newerBuild(b(4, at(5)), b(3, at(5))) || newerBuild(b(4, nil), b(3, at(5))) {
		t.Error("newerBuild ordering is wrong")
	}
	x := b(1, nil)
	if deployImage(x) != "r/x:1" {
		t.Error(deployImage(x))
	}
	x.Status.Digest = "sha256:ab"
	if deployImage(x) != "r/x:1@sha256:ab" {
		t.Error(deployImage(x))
	}
}

func TestGitHubInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v3/app/installations/42/access_tokens" {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parts := strings.Split(jwt, ".")
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if len(parts) != 3 || rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
			http.Error(w, `{"message":"bad jwt"}`, http.StatusUnauthorized)
			return
		}
		claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var c struct {
			Iss      string `json:"iss"`
			Iat, Exp int64
		}
		_ = json.Unmarshal(claims, &c)
		if c.Iss != "7" || c.Exp-c.Iat > 600 {
			http.Error(w, `{"message":"bad claims"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"token":"ghs_minted","expires_at":"2026-10-04T13:00:00Z"}`)
	}))
	defer srv.Close()

	conn := &kwerftv1.GitConnection{Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, URL: srv.URL,
		Auth: kwerftv1.GitAuthGitHubApp, GitHubApp: &kwerftv1.GitHubAppSettings{AppID: 7, InstallationID: 42}}}
	mint := GitHubInstallationToken(srv.Client())
	tok, err := mint(context.Background(), conn, keyPEM)
	if err != nil || tok != "ghs_minted" {
		t.Fatalf("token %q err %v", tok, err)
	}
	conn.Spec.GitHubApp.InstallationID = 43
	var perm permanentError
	if _, err := mint(context.Background(), conn, keyPEM); !errors.As(err, &perm) {
		t.Errorf("unknown installation: %v", err)
	}
	if _, err := mint(context.Background(), conn, []byte("not a key")); !errors.As(err, &perm) {
		t.Errorf("bad key: %v", err)
	}
	if got := githubAPIBase("https://github.com"); got != "https://api.github.com" {
		t.Error(got)
	}
}

// fakeRegistry is the part of the OCI distribution API RegistryKeeper uses.
type fakeRegistry struct {
	mu        sync.Mutex
	manifests map[string][]byte // digest → body
	tags      map[string]string // tag → digest
	// user and password, when set, are required for writes, as zot does
	// with Kwerft's access control (reads stay anonymous).
	user, password string
}

func newRegistryServer(t *testing.T, f *fakeRegistry) *httptest.Server {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeRegistry) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); f.user != "" && r.Method != http.MethodGet && r.Method != http.MethodHead && (u != f.user || p != f.password) {
		w.Header().Set("WWW-Authenticate", `Basic realm="zot"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	rest, ok := strings.CutPrefix(r.URL.Path, "/v2/shop/api/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case rest == "tags/list":
		var tags []string
		for t := range f.tags {
			tags = append(tags, t)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "shop/api", "tags": tags})
	case strings.HasPrefix(rest, "manifests/"):
		ref := strings.TrimPrefix(rest, "manifests/")
		switch r.Method {
		case http.MethodGet:
			body, ok := f.manifests[ref]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
			_, _ = w.Write(body)
		case http.MethodPut:
			if r.Header.Get("Content-Type") != "application/vnd.oci.image.index.v1+json" {
				http.Error(w, "bad media type", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			for d, m := range f.manifests {
				if string(m) == string(body) {
					f.tags[ref] = d
				}
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			delete(f.tags, ref)
			w.WriteHeader(http.StatusAccepted)
		}
	default:
		http.NotFound(w, r)
	}
}

func TestRegistryKeeperTagsRevisionImages(t *testing.T) {
	d1 := "sha256:" + strings.Repeat("1", 64)
	d2 := "sha256:" + strings.Repeat("2", 64)
	reg := &fakeRegistry{
		manifests: map[string][]byte{d1: []byte(`{"m":1}`), d2: []byte(`{"m":2}`)},
		tags:      map[string]string{"aaa": d1, "bbb": d2, "buildcache": d2, keepTag("sha256:" + strings.Repeat("9", 64)): d1},
	}
	srv := httptest.NewServer(reg)
	defer srv.Close()
	k := &RegistryKeeper{URL: srv.URL, HTTP: srv.Client()}
	repo := builds.ImageRepository("shop", "api")
	history := []kwerftv1.AppRevision{
		{Number: 3, Image: repo + ":bbb@" + d2},
		{Number: 2, Image: "ghcr.io/acme/api:1@" + d1}, // not ours
		{Number: 1, Image: repo + ":aaa@" + d1},
	}
	if err := k.Keep(context.Background(), "shop", "api", history); err != nil {
		t.Fatal(err)
	}
	if reg.tags[keepTag(d1)] != d1 || reg.tags[keepTag(d2)] != d2 || len(reg.tags) != 5 {
		t.Errorf("tags = %v", reg.tags)
	}
	// Revision 1 leaves the history: its keep tag goes, other tags stay.
	if err := k.Keep(context.Background(), "shop", "api", history[:1]); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.tags[keepTag(d1)]; ok || reg.tags["aaa"] != d1 || reg.tags[keepTag(d2)] != d2 {
		t.Errorf("tags = %v", reg.tags)
	}
}

func TestBuildContainerAppArmor(t *testing.T) {
	r := testRun("dockerfile", "ssh")
	build := renderJob(t, r).Spec.Template.Spec.Containers[0]
	if p := build.SecurityContext.AppArmorProfile; p == nil || p.Type != corev1.AppArmorProfileTypeUnconfined {
		t.Errorf("without a profile: %+v", p)
	}
	r.appArmor = "kwerft-buildkit"
	build = renderJob(t, r).Spec.Template.Spec.Containers[0]
	if p := build.SecurityContext.AppArmorProfile; p == nil || p.Type != corev1.AppArmorProfileTypeLocalhost || p.LocalhostProfile == nil || *p.LocalhostProfile != "kwerft-buildkit" {
		t.Errorf("with the installer's profile: %+v", p)
	}
	// Only the build container needs it; clone and prepare stay confined.
	for _, c := range renderJob(t, r).Spec.Template.Spec.InitContainers {
		if c.SecurityContext != nil && c.SecurityContext.AppArmorProfile != nil {
			t.Errorf("%s has AppArmor %+v", c.Name, c.SecurityContext.AppArmorProfile)
		}
	}
}
