// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeRemote plays a server: cloud-init, the installer (which "installs"
// a version into the fake console, joins a node, or restores), the setup
// token, and the scripts the runs send.
type fakeRemote struct {
	mu          sync.Mutex
	console     *fakeConsole
	name        string // the server's name, its hostname
	cmds        []string
	exit        map[string]int  // installer exit code by version
	noStage     map[string]bool // versions whose installer lacks --acme-server
	hang        bool            // the installer never finishes
	versions    []string        // installed, in order
	pin         string          // the installer's K3S_VERSION
	noFaults    bool            // the chart has no e2e.faults
	trusted     bool            // Let's Encrypt staging is trusted
	restoreFail bool            // --restore cannot read the backups
	uploads     map[string]string
	registry    string // the registry check's answer; "" all as it should be
}

var (
	installCmd = regexp.MustCompile(`^((?:[A-Z0-9_]+='[^']*' )*)bash '/root/kwerft-install-([^']+)\.sh'(.*)$`)
	envRE      = regexp.MustCompile(`([A-Z0-9_]+)='([^']*)'`)
	quotedRE   = regexp.MustCompile(`'([^']*)'`)
	annotateRE = regexp.MustCompile(`annotate upgrades\.kwerft\.dev '([^']+)'`)
)

func (f *fakeRemote) run(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error) {
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	switch {
	case strings.HasPrefix(cmd, "cloud-init status"):
		fmt.Fprintln(stdout, "Ubuntu 26.04 LTS")
	case strings.HasPrefix(cmd, "curl -fsSL"):
	case strings.HasPrefix(cmd, "grep -q -e '--acme-server'"):
		v := strings.TrimSuffix(strings.TrimPrefix(cmd[strings.Index(cmd, "kwerft-install-"):], "kwerft-install-"), ".sh'")
		if f.noStage[v] {
			return 1, nil
		}
	case strings.HasPrefix(cmd, "sed -n 's/^K3S_VERSION="):
		fmt.Fprintln(stdout, f.pin)
	case installCmd.MatchString(cmd):
		return f.install(ctx, cmd, stdout, stderr)
	case cmd == "cat /etc/kwerft/setup-token":
		fmt.Fprintln(stdout, f.console.token)
	case strings.Contains(cmd, "helm upgrade kwerft"):
		if f.noFaults && strings.Contains(cmd, "e2e.faults=true") {
			fmt.Fprintln(stderr, "the console runs without --upgrade-faults: the chart of 0.5.0 does not have this value")
			return 1, nil
		}
		f.console.mu.Lock()
		f.console.faults = f.console.faults || strings.Contains(cmd, "e2e.faults=true")
		if m := regexp.MustCompile(`'upgrades\.installBaseURL=([^']+)'`).FindStringSubmatch(cmd); m != nil {
			f.console.installBase = m[1]
		}
		f.console.mu.Unlock()
	case strings.Contains(cmd, "kwerft.dev/e2e-fault="):
		m := annotateRE.FindStringSubmatch(cmd)
		if m == nil {
			return 1, nil
		}
		if err := f.console.setFault(m[1], faultInstall); err != nil {
			fmt.Fprintln(stderr, err)
			return 1, nil
		}
		fmt.Fprintf(stdout, "runner %s carries --fault=install\n", runnerJobName(m[1]))
	case strings.Contains(cmd, "update-ca-certificates"):
		f.mu.Lock()
		f.trusted = true
		f.mu.Unlock()
	case cmd == registryCheckScript:
		f.mu.Lock()
		answer := cmpOr(f.registry, "own=200 other=403 anonymous=401 pull=200")
		f.mu.Unlock()
		fmt.Fprintln(stdout, answer)
	}
	return 0, nil
}

func (f *fakeRemote) install(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error) {
	m := installCmd.FindStringSubmatch(cmd)
	env := map[string]string{}
	for _, kv := range envRE.FindAllStringSubmatch(m[1], -1) {
		env[kv[1]] = kv[2]
	}
	v := m[2]
	var args []string
	for _, q := range quotedRE.FindAllStringSubmatch(m[3], -1) {
		args = append(args, q[1])
	}
	value := func(flag string) string {
		if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if f.hang {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	fmt.Fprintf(stdout, "▸ Kwerft installer %s\n✓ Preflight\n", v)
	if code := f.exit[v]; code != 0 {
		fmt.Fprintf(stderr, "✗ Stage \"Kubernetes\" failed\n")
		return code, nil
	}
	k3s := cmpOr(env["KWERFT_K3S_VERSION"], f.pin)
	switch {
	case slices.Contains(args, "--join"):
		f.mu.Lock()
		trusted := f.trusted
		f.mu.Unlock()
		if !trusted {
			fmt.Fprintln(stderr, "curl: (60) SSL certificate problem: unable to get local issuer certificate")
			return 20, nil
		}
		if value("--join") != "https://"+f.console.domain || !f.console.spendJoinToken(value("--token")) {
			fmt.Fprintln(stderr, "✗ Could not reach the console or the join token was rejected")
			return 20, nil
		}
		f.console.addNode(f.name, "worker", k3s)
		fmt.Fprintf(stdout, "✓ Join · k3s %s agent joined\n", k3s)
	case slices.Contains(args, "--restore"):
		if slices.Contains(args, "--domain") {
			fmt.Fprintln(stderr, "--domain would move the console off the backup's hostname")
			return 2, nil
		}
		if err := f.checkRestore(value("--config")); err != nil {
			fmt.Fprintln(stderr, "✗ Restore: "+err.Error())
			return 60, nil
		}
		f.console.restored(v)
		fmt.Fprintf(stdout, "✓ Restore · kwerft-cluster restored\n  DNS         point %s and the apps hostnames at this server: 203.0.113.11\n", f.console.domain)
	default:
		f.console.installed(v)
		f.console.addNode(f.name, "control-plane", k3s)
	}
	f.mu.Lock()
	f.versions = append(f.versions, v)
	f.mu.Unlock()
	return 0, nil
}

// checkRestore is what install.sh --restore needs: the config's backups
// block, the key files it names, and a backup under the prefix.
func (f *fakeRemote) checkRestore(config string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.restoreFail {
		return errors.New("Cannot read the backups")
	}
	y, ok := f.uploads[config]
	if !ok {
		return fmt.Errorf("--config %s not readable", config)
	}
	field := func(k string) string {
		m := regexp.MustCompile(`(?m)^  ` + k + `: (.*)$`).FindStringSubmatch(y)
		if m == nil {
			return ""
		}
		return m[1]
	}
	c := f.console
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.target == nil:
		return errors.New("no backups were ever configured")
	case field("endpoint") != c.target.Endpoint || field("bucket") != c.target.Bucket || field("prefix") != c.target.Prefix:
		return fmt.Errorf("another bucket or prefix than the backups': %q", y)
	case strings.TrimSpace(f.uploads[field("accessKeyFile")]) != c.s3.creds.AccessKey || strings.TrimSpace(f.uploads[field("secretKeyFile")]) != c.s3.creds.SecretKey:
		return errors.New("the access keys do not open the bucket")
	case strings.TrimSpace(f.uploads[field("recoveryKeyFile")]) != c.recoveryKey:
		return errors.New("no complete Cluster backup readable with this recovery key")
	case len(c.s3.keys(c.target.Prefix+"/velero/backups/")) == 0:
		return errors.New("there are no backups under the prefix")
	}
	return nil
}

func (f *fakeRemote) upload(_ context.Context, path string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds = append(f.cmds, "upload "+path)
	if f.uploads == nil {
		f.uploads = map[string]string{}
	}
	f.uploads[path] = string(data)
	return nil
}

func (f *fakeRemote) close() error { return nil }

func (f *fakeRemote) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cmds...)
}

type harness struct {
	cloud   *fakeCloud
	console *fakeConsole
	remote  *fakeRemote // the first server's
	runner  *runner
	log     *bytes.Buffer
	dials   int
	masked  []string

	mu      sync.Mutex
	remotes map[string]*fakeRemote // by address
}

func newHarness(t *testing.T, version, from string) *harness {
	t.Helper()
	return newHarnessWith(t, config{Version: version, From: from})
}

// newHarnessWith runs base (version, from and the mode's fields) against
// the fakes.
func newHarnessWith(t *testing.T, base config) *harness {
	t.Helper()
	h := &harness{cloud: newFakeCloud(t), log: &bytes.Buffer{}, remotes: map[string]*fakeRemote{}}
	h.console = newFakeConsole(t, newTestCA(t, "(STAGING) Let's Encrypt"), "203.0.113.10")
	h.remote = h.remoteFor("203.0.113.10")
	h.cloud.onDelete = func(ip string) {
		if ip == "203.0.113.10" {
			h.console.gone()
		}
	}
	cfg := base
	cfg.Token, cfg.RunID = "test-token", cmpOr(base.RunID, "42-1-fresh")
	cfg.ServerTypes, cfg.Locations, cfg.Images = []string{"cx33", "cx43"}, []string{"nbg1", "fsn1"}, []string{"ubuntu-26.04", "ubuntu-24.04"}
	cfg.InstallerURL, cfg.GitRepo, cfg.GitBranch = "https://example.test/v{version}/install.sh", "https://github.com/traefik/whoami", "master"
	cfg.ChartRef, cfg.StagingRoots = defaultChartRef, defaultStagingRoots
	cfg.Timeout, cfg.InstallTimeout, cfg.Poll = time.Minute, 10*time.Second, time.Millisecond
	if cfg.K3s && cfg.Workers == 0 {
		cfg.Workers = 2
	}
	if err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	h.runner = &runner{
		cfg: cfg, cloud: h.cloud.client(),
		dial: func(_ context.Context, addr string, k *runKeys) (remote, error) {
			h.mu.Lock()
			h.dials++
			dials := h.dials
			h.mu.Unlock()
			ip, port, _ := strings.Cut(addr, ":")
			if !strings.HasPrefix(ip, "203.0.113.") || port != "22" {
				return nil, fmt.Errorf("dialled %s", addr)
			}
			if dials < 3 {
				return nil, errors.New("connection refused")
			}
			rem := h.remoteFor(ip)
			rem.mu.Lock()
			if rem.name == "" {
				rem.name = h.cloud.serverByIP(ip)
			}
			rem.mu.Unlock()
			return rem, nil
		},
		transport: h.console.client,
		log:       h.log,
		now:       time.Now,
		mask:      func(s string) { h.mu.Lock(); h.masked = append(h.masked, s); h.mu.Unlock() },
		rep:       &report{Title: "Kwerft e2e"},
	}
	return h
}

// remoteFor is the fake server at ip.
func (h *harness) remoteFor(ip string) *fakeRemote {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.remotes[ip]; ok {
		return r
	}
	r := &fakeRemote{console: h.console, exit: map[string]int{}, noStage: map[string]bool{}, pin: "v1.37.1+k3s1", uploads: map[string]string{}}
	h.remotes[ip] = r
	return r
}

func (h *harness) markdown() string {
	var b bytes.Buffer
	h.runner.rep.markdown(&b)
	return b.String()
}

func (h *harness) assertCleanedUp(t *testing.T) {
	t.Helper()
	if s, k := h.cloud.counts(); s != 0 || k != 0 {
		t.Errorf("left behind: %d server(s), %d SSH key(s)", s, k)
	}
	if n := h.cloud.networkCount(); n != 0 {
		t.Errorf("left behind: %d network(s)", n)
	}
	if !h.runner.rep.CleanupOK {
		t.Errorf("cleanup not reported OK: %v", h.runner.rep.Cleanup)
	}
}

func resultsByName(rep *report) map[string]result {
	out := map[string]result{}
	for _, r := range rep.Results {
		out[r.Name] = r
	}
	return out
}

func TestFreshInstallPassesAndCleansUp(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.runner.run(context.Background())

	md := h.markdown()
	if !h.runner.rep.passed() {
		t.Fatalf("run failed:\n%s\nlog:\n%s", md, h.log)
	}
	h.assertCleanedUp(t)
	res := resultsByName(h.runner.rep)
	for _, name := range []string{"Create server", "SSH", "Install v0.5.0", "Console on HTTPS", "Owner from the setup token, sign in",
		"Project and apps created", "App on HTTPS", "Task runs and restarts web", "Git build deploys", "Registry refuses other projects' pushes",
		"Log search", "Metrics", "Crash loop alert"} {
		if res[name].Status != pass {
			t.Errorf("%s: %+v", name, res[name])
		}
	}
	if !strings.Contains(res["Console on HTTPS"].Detail, "(STAGING) Let's Encrypt") {
		t.Errorf("the certificate's issuer is not reported: %q", res["Console on HTTPS"].Detail)
	}
	if !strings.Contains(md, "cx33 (4 vCPU, 8 GB) in nbg1, ubuntu-26.04") || !strings.Contains(md, "€0.0136") {
		t.Errorf("server or cost missing from the summary:\n%s", md)
	}

	// The installer ran with the sslip.io name, staging and the version.
	var install string
	for _, c := range h.remote.commands() {
		if installCmd.MatchString(c) {
			install = c
		}
	}
	want := "bash '/root/kwerft-install-0.5.0.sh' '--domain' '203.0.113.10.sslip.io' '--version' '0.5.0' '--yes' '--acme-server' 'staging'"
	if install != want {
		t.Errorf("installer command\n got %s\nwant %s", install, want)
	}
	// Secrets are masked in CI logs.
	if len(h.masked) != 2 || h.masked[0] != h.console.token {
		t.Errorf("masked %v", h.masked)
	}

	// Labels and the pinned host key went to Hetzner.
	h.cloud.mu.Lock()
	defer h.cloud.mu.Unlock()
	if len(h.cloud.userData) != 1 {
		t.Fatalf("servers created: %d", len(h.cloud.userData))
	}
	for _, ud := range h.cloud.userData {
		if !strings.Contains(ud, h.runner.keys.hostPublic) || !strings.Contains(ud, "BEGIN OPENSSH PRIVATE KEY") {
			t.Errorf("user data does not pin the host key:\n%s", ud)
		}
	}
}

func TestInstallerFailureStillDestroys(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.remote.exit["0.5.0"] = 30
	h.runner.run(context.Background())

	if h.runner.rep.passed() {
		t.Fatal("a failed install passed")
	}
	h.assertCleanedUp(t)
	res := resultsByName(h.runner.rep)
	if r := res["Install v0.5.0"]; r.Status != fail || !strings.Contains(r.Detail, "installer exited 30 (Kubernetes)") {
		t.Errorf("install result: %+v", r)
	}
	if _, ran := res["App on HTTPS"]; ran {
		t.Error("checks ran after a failed install")
	}
	if !strings.Contains(h.runner.rep.Log, "Stage \"Kubernetes\" failed") {
		t.Errorf("the installer's output is not in the report: %q", h.runner.rep.Log)
	}
}

func TestTimeoutStillDestroys(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.remote.hang = true
	h.runner.cfg.InstallTimeout = time.Hour
	h.runner.cfg.Timeout = 300 * time.Millisecond
	h.runner.run(context.Background())

	if h.runner.rep.passed() {
		t.Fatal("a timed-out run passed")
	}
	h.assertCleanedUp(t)
	if !strings.Contains(strings.Join(h.runner.rep.Notes, " "), "stopped early") {
		t.Errorf("notes: %v", h.runner.rep.Notes)
	}
}

func TestCancelStillDestroys(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.remote.hang = true
	h.runner.cfg.InstallTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	h.runner.run(ctx)
	h.assertCleanedUp(t)
}

func TestFailedCheckFailsRunButOthersReport(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.console.buildFail = true
	h.console.noAlert = true
	h.runner.cfg.Timeout = 3 * time.Second
	h.runner.run(context.Background())

	if h.runner.rep.passed() {
		t.Fatal("passed with a failed build")
	}
	h.assertCleanedUp(t)
	res := resultsByName(h.runner.rep)
	if r := res["Git build deploys"]; r.Status != fail || !strings.Contains(r.Detail, "Dockerfile not found") {
		t.Errorf("build: %+v", r)
	}
	if res["Metrics"].Status != pass || res["Log search"].Status != pass {
		t.Errorf("independent checks should still pass: %+v %+v", res["Metrics"], res["Log search"])
	}
	if res["Crash loop alert"].Status == pass {
		t.Errorf("alert: %+v", res["Crash loop alert"])
	}
	// The report keeps a fixed order whatever finished first.
	var names []string
	for _, r := range h.runner.rep.Results {
		names = append(names, r.Name)
	}
	if got := strings.Join(names[len(names)-7:], ","); got != "App on HTTPS,Task runs and restarts web,Log search,Git build deploys,Registry refuses other projects' pushes,Metrics,Crash loop alert" {
		t.Errorf("order: %s", got)
	}
}

func TestOpenRegistryFailsTheCheck(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.remote.registry = "own=200 other=202 anonymous=202 pull=200"
	h.runner.run(context.Background())

	if h.runner.rep.passed() {
		t.Fatal("passed although the registry takes anyone's pushes")
	}
	res := resultsByName(h.runner.rep)
	if r := res["Registry refuses other projects' pushes"]; r.Status != fail || !strings.Contains(r.Detail, "other=202") {
		t.Errorf("registry: %+v", r)
	}
	if res["Git build deploys"].Status != pass {
		t.Errorf("build: %+v", res["Git build deploys"])
	}
	h.assertCleanedUp(t)

	// A version without per-project credentials is not held to them.
	h = newHarness(t, "0.5.0", "")
	h.remote.registry = "no-credential"
	h.runner.run(context.Background())
	if r := resultsByName(h.runner.rep)["Registry refuses other projects' pushes"]; r.Status != pass || !strings.Contains(r.Detail, "not checked") {
		t.Errorf("registry: %+v", r)
	}
}

func TestUpgradeFromAnInstallerWithoutStaging(t *testing.T) {
	h := newHarness(t, "0.5.0", "0.4.0")
	h.remote.noStage["0.4.0"] = true
	h.runner.run(context.Background())

	if !h.runner.rep.passed() {
		t.Fatalf("run failed:\n%s\nlog:\n%s", h.markdown(), h.log)
	}
	h.assertCleanedUp(t)
	if got := strings.Join(h.remote.versions, ","); got != "0.4.0,0.5.0" {
		t.Errorf("installed %s", got)
	}
	cmds := strings.Join(h.remote.commands(), "\n")
	if !strings.Contains(cmds, "bash '/root/kwerft-install-0.4.0.sh' '--domain' '203.0.113.10.sslip.io' '--version' '0.4.0' '--yes'\n") {
		t.Errorf("the old installer got --acme-server:\n%s", cmds)
	}
	if strings.Count(cmds, "kc patch clusterissuer letsencrypt") != 1 {
		t.Errorf("the staging fallback should run once, after the old installer:\n%s", cmds)
	}
	res := resultsByName(h.runner.rep)
	for _, name := range []string{"Install v0.4.0", "App before the upgrade", "Upgrade to v0.5.0", "After the upgrade", "Crash loop alert"} {
		if res[name].Status != pass {
			t.Errorf("%s: %+v", name, res[name])
		}
	}
	if !strings.Contains(res["Install v0.4.0"].Detail, "switched to staging") {
		t.Errorf("detail: %q", res["Install v0.4.0"].Detail)
	}
	if !strings.Contains(h.markdown(), "Upgrade from v0.4.0 to v0.5.0") {
		t.Error("scenario missing from the summary")
	}
}

func TestUpgradeChecksTheVersion(t *testing.T) {
	h := newHarness(t, "0.5.0", "0.4.0")
	// The upgrade "succeeds" but the console keeps running the old version.
	h.remote.exit["0.5.0"] = 0
	orig := h.console
	h.runner.transport = orig.client
	h.runner.cfg.Timeout = 2 * time.Second
	stuck := &fakeRemoteStuck{fakeRemote: h.remote, stay: "0.4.0"}
	h.runner.dial = func(context.Context, string, *runKeys) (remote, error) { return stuck, nil }
	h.runner.run(context.Background())

	res := resultsByName(h.runner.rep)
	if r := res["After the upgrade"]; r.Status != fail || !strings.Contains(r.Detail, `reports version "0.4.0", want 0.5.0`) {
		t.Errorf("after the upgrade: %+v", r)
	}
	h.assertCleanedUp(t)
}

// fakeRemoteStuck installs every version but the console stays at one.
type fakeRemoteStuck struct {
	*fakeRemote
	stay string
}

func (f *fakeRemoteStuck) run(ctx context.Context, cmd string, stdout, stderr io.Writer) (int, error) {
	code, err := f.fakeRemote.run(ctx, cmd, stdout, stderr)
	if installCmd.MatchString(cmd) {
		f.console.installed(f.stay)
	}
	return code, err
}

func TestServerTypeAndLocationFallback(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.cloud.unavailable["cx33/nbg1"] = true
	h.cloud.unavailable["cx33/fsn1"] = true
	delete(h.cloud.images, "ubuntu-26.04")
	h.runner.run(context.Background())

	if !h.runner.rep.passed() {
		t.Fatalf("run failed:\n%s", h.markdown())
	}
	md := h.markdown()
	if !strings.Contains(md, "cx43 (8 vCPU, 16 GB) in nbg1, ubuntu-24.04") {
		t.Errorf("fallback not used:\n%s", md)
	}
	if !strings.Contains(md, "cx33 in nbg1: resource_unavailable; cx33 in fsn1: resource_unavailable") {
		t.Errorf("skipped combinations not noted:\n%s", md)
	}
}

// A project at its resource limits (other runs hold servers) is asked again
// until they free up.
func TestServerWaitsForProjectLimits(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.cloud.atLimit = 3
	h.runner.run(context.Background())
	if !h.runner.rep.passed() {
		t.Fatalf("run failed:\n%s", h.markdown())
	}
	if h.cloud.atLimit != 0 || !strings.Contains(h.markdown(), "for the project's resource limits") {
		t.Errorf("limit left %d; report:\n%s", h.cloud.atLimit, h.markdown())
	}
}

func TestNoServerAvailable(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	for _, c := range []string{"cx33/nbg1", "cx33/fsn1", "cx43/nbg1", "cx43/fsn1"} {
		h.cloud.unavailable[c] = true
	}
	// A server of this run whose create answer never arrived.
	h.cloud.mu.Lock()
	h.cloud.servers[7] = &hServer{ID: 7, Name: "kwerft-e2e-42-1-fresh", Created: time.Now(), Labels: map[string]string{labelE2E: "true", labelRun: "42-1-fresh"}}
	h.cloud.mu.Unlock()
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed without a server")
	}
	// The SSH key was uploaded before; it must go again, and the lost server too.
	h.assertCleanedUp(t)
	if !strings.Contains(strings.Join(h.runner.rep.Cleanup, "; "), "deleted server kwerft-e2e-42-1-fresh (id 7") {
		t.Errorf("cleanup: %v", h.runner.rep.Cleanup)
	}
}

func TestFailedDeletionFailsTheRun(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.cloud.failDeletes = true
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed although the server was not deleted")
	}
	md := h.markdown()
	if !strings.Contains(md, "NOT deleted") || !strings.Contains(md, "incomplete") {
		t.Errorf("summary does not flag the leftover:\n%s", md)
	}
}

func TestDeletionIsAwaited(t *testing.T) {
	h := newHarness(t, "0.5.0", "")
	h.cloud.pollsUntilGone = 3
	h.runner.run(context.Background())
	h.assertCleanedUp(t)
}

func TestHostKeyPinning(t *testing.T) {
	k, err := newRunKeys("kwerft-e2e-test")
	if err != nil {
		t.Fatal(err)
	}
	ci := cloudInit(k)
	for _, want := range []string{"#cloud-config\n", "ssh_deletekeys: true\n", "  ed25519_private: |\n    -----BEGIN OPENSSH PRIVATE KEY-----\n", "  ed25519_public: ssh-ed25519 "} {
		if !strings.Contains(ci, want) {
			t.Errorf("cloud-init lacks %q:\n%s", want, ci)
		}
	}
	// The key in cloud-init is the one the harness pins.
	var pemLines []string
	for _, l := range strings.Split(ci, "\n") {
		if strings.HasPrefix(l, "    ") {
			pemLines = append(pemLines, strings.TrimPrefix(l, "    "))
		}
	}
	parsed, err := ssh.ParsePrivateKey([]byte(strings.Join(pemLines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.PublicKey().Marshal(), k.host.PublicKey().Marshal()) {
		t.Error("cloud-init installs a different host key than the one pinned")
	}
	if !strings.HasPrefix(k.clientPub, "ssh-ed25519 ") || !strings.HasSuffix(k.clientPub, " kwerft-e2e-test") {
		t.Errorf("client key %q", k.clientPub)
	}
}

func TestConfigValidate(t *testing.T) {
	base := config{Version: "0.5.0", RunID: "1-1-fresh", ServerTypes: []string{"cx33"}, Locations: []string{"nbg1"}, Images: []string{"ubuntu-26.04"}}
	for name, tc := range map[string]struct {
		mod  func(*config)
		want string
	}{
		"ok":            {func(*config) {}, ""},
		"no version":    {func(c *config) { c.Version = "" }, "required"},
		"bad version":   {func(c *config) { c.Version = "latest" }, "not a release version"},
		"from newer":    {func(c *config) { c.From = "0.6.0" }, "must be older"},
		"from same":     {func(c *config) { c.From = "0.5.0" }, "must be older"},
		"bad run id":    {func(c *config) { c.RunID = "a b" }, "label value"},
		"long run id":   {func(c *config) { c.RunID = strings.Repeat("a", 60) }, "label value"},
		"no locations":  {func(c *config) { c.Locations = nil }, "must not be empty"},
		"rc from final": {func(c *config) { c.Version, c.From = "0.5.0-rc.1", "0.4.0" }, ""},
	} {
		c := base
		tc.mod(&c)
		err := c.validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestDryRunPrintsThePlan(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "")
	var out bytes.Buffer
	if err := cmdRun(context.Background(), []string{"-version", "v0.5.0", "-from", "0.4.0", "-run-id", "7-1-upgrade", "-dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"Dry run — Upgrade from v0.4.0 to v0.5.0, run 7-1-upgrade. Nothing is created.",
		`upload the public key as "kwerft-e2e-7-1-upgrade" with labels kwerft-e2e=true, run=7-1-upgrade`,
		"cx33, cx43 in nbg1, fsn1, hel1",
		"https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v0.4.0/install.sh",
		"--acme-server staging --version 0.5.0",
		"Always, also on failure, timeout or cancel: delete the server and the SSH key",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("plan lacks %q:\n%s", want, s)
		}
	}
	// Without -dry-run, no token is a usage error.
	err := cmdRun(context.Background(), []string{"-version", "0.5.0"}, io.Discard)
	var u usageError
	if !errors.As(err, &u) || !strings.Contains(err.Error(), "HCLOUD_TOKEN") {
		t.Errorf("without a token: %v", err)
	}
}
