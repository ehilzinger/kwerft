package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

func mustPass(t *testing.T, h *harness) {
	t.Helper()
	if !h.runner.rep.passed() {
		t.Fatalf("run failed:\n%s\nlog:\n%s", h.markdown(), h.log)
	}
	h.assertCleanedUp(t)
}

func wantResults(t *testing.T, h *harness, st status, names ...string) map[string]result {
	t.Helper()
	res := resultsByName(h.runner.rep)
	for _, n := range names {
		if r, ok := res[n]; !ok || r.Status != st {
			t.Errorf("%s: %+v (want %s)", n, r, st)
		}
	}
	return res
}

func (h *harness) allCommands() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.remotes {
		out = append(out, r.commands()...)
	}
	return strings.Join(out, "\n")
}

// ---- via the console ----------------------------------------------------------------

func TestUpgradeThroughTheConsole(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", ViaConsole: true})
	h.runner.run(context.Background())
	mustPass(t, h)

	res := wantResults(t, h, pass, "Install v0.5.0", "Console on HTTPS", "App before the upgrade", "Chart values for this run",
		"Upgrade to v0.6.0 through the console", "App availability during the upgrade", "After the upgrade", "Crash loop alert")
	up := res["Upgrade to v0.6.0 through the console"].Detail
	for _, want := range []string{"kwerft-0-6-0-x7k2p: Pending → Preflight → Backup → Running → Verifying → Succeeded", "3 of 3 installer stages done", "reconnected 1 time(s)"} {
		if !strings.Contains(up, want) {
			t.Errorf("upgrade detail lacks %q: %s", want, up)
		}
	}
	if d := res["App availability during the upgrade"].Detail; !strings.Contains(d, "https://hello.203.0.113.10.sslip.io/: longest gap none, 0 of") {
		t.Errorf("availability: %s", d)
	}
	// Only the old release was installed by the installer; the console did the rest.
	if got := strings.Join(h.remote.versions, ","); got != "0.5.0" {
		t.Errorf("installer runs: %s", got)
	}
	// The run's install repository is not the console's default: the chart says so.
	if d := res["Chart values for this run"].Detail; !strings.Contains(d, "upgrades.installBaseURL=https://example.test") || strings.Contains(d, "e2e.faults") {
		t.Errorf("chart values: %s", d)
	}
	if h.console.installBase != "https://example.test" || h.console.faults {
		t.Errorf("console: base %q, faults %v", h.console.installBase, h.console.faults)
	}
	if !strings.Contains(h.markdown(), "Upgrade from v0.5.0 to v0.6.0 through the console") {
		t.Error("scenario missing")
	}
}

// A release candidate's run upgrades from the candidate before, which the
// console only offers on the edge channel.
func TestUpgradeToCandidateThroughTheConsole(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0-rc.3", From: "0.6.0-rc.2", ViaConsole: true})
	h.runner.run(context.Background())
	mustPass(t, h)
	wantResults(t, h, pass, "Install v0.6.0-rc.2", "Edge channel", "Upgrade to v0.6.0-rc.3 through the console", "After the upgrade")
	if h.console.channel != "edge" {
		t.Errorf("channel = %q", h.console.channel)
	}
}

func TestForcedFailureRollsBack(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", ViaConsole: true, Fault: true, RunID: "42-1-fault"})
	h.runner.run(context.Background())
	mustPass(t, h)

	name := "Upgrade to v0.6.0 through the console, failing after the Kwerft stage"
	res := wantResults(t, h, pass, "Chart values for this run", name, "App availability during the upgrade", "After the rollback")
	d := res[name].Detail
	for _, want := range []string{"→ RollingBack → RolledBack", "runner kwerft-0-6-0-x7k2p-runner carries --fault=install", "Fault injected (e2e)"} {
		if !strings.Contains(d, want) {
			t.Errorf("upgrade detail lacks %q: %s", want, d)
		}
	}
	if d := res["After the rollback"].Detail; !strings.Contains(d, "version 0.5.0 again") || !strings.Contains(d, "hello-0 (0 restarts)") {
		t.Errorf("after: %s", d)
	}
	if !strings.Contains(res["Chart values for this run"].Detail, "e2e.faults=true") || !h.console.faults {
		t.Errorf("faults not enabled: %s", res["Chart values for this run"].Detail)
	}
	if v, _ := h.console.state(); v != "0.5.0" {
		t.Errorf("console on %s", v)
	}
	// The forced failure does not run the suite.
	if _, ok := res["Crash loop alert"]; ok {
		t.Error("the suite ran after the rollback")
	}
	cmds := h.allCommands()
	if !strings.Contains(cmds, "helm upgrade kwerft 'oci://ghcr.io/ehilzinger/charts/kwerft' --version '0.5.0' --namespace kwerft-system --reuse-values --set 'e2e.faults=true'") {
		t.Errorf("helm command:\n%s", cmds)
	}
}

func TestForcedFailureNeedsTheChartValue(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", Fault: true, ViaConsole: true})
	h.remote.noFaults = true
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed without fault injection")
	}
	h.assertCleanedUp(t)
	res := resultsByName(h.runner.rep)
	if r := res["Chart values for this run"]; r.Status != fail || !strings.Contains(r.Detail, "does not have this value") {
		t.Errorf("chart values: %+v", r)
	}
	if _, ok := res["App availability during the upgrade"]; ok {
		t.Error("upgraded without fault injection")
	}
}

func TestRollbackThatTouchedApps(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", Fault: true, ViaConsole: true})
	h.console.touchApps = true
	h.runner.run(context.Background())
	res := resultsByName(h.runner.rep)
	if r := res["After the rollback"]; r.Status != fail || !strings.Contains(r.Detail, "pods changed during the failed upgrade: before [hello-0 (0 restarts)], after [hello-1 (0 restarts)]") {
		t.Errorf("after the rollback: %+v", r)
	}
	h.assertCleanedUp(t)
}

func TestUnexpectedRollbackFails(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", ViaConsole: true})
	h.console.failUpgrade = true
	h.console.upgradeLog = "▸ Kwerft installer 0.6.0\n✗ Verify: the console did not come up\n"
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed although the upgrade rolled back")
	}
	h.assertCleanedUp(t)
	r := resultsByName(h.runner.rep)["Upgrade to v0.6.0 through the console"]
	if r.Status != fail || !strings.Contains(r.Detail, "ended RolledBack, want Succeeded: Verify The console did not come up on 0.6.0.") {
		t.Errorf("upgrade: %+v", r)
	}
	if !strings.Contains(h.runner.rep.Log, "✗ Verify") {
		t.Errorf("the installer log is not in the report: %q", h.runner.rep.Log)
	}
}

func TestBlockedPreflight(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", From: "0.5.0", ViaConsole: true})
	h.console.preflightFail = "0.6.0 does not allow upgrades from 0.5.0."
	h.runner.run(context.Background())
	r := resultsByName(h.runner.rep)["Upgrade to v0.6.0 through the console"]
	if r.Status != fail || !strings.Contains(r.Detail, "the console refused (HTTP 409): The preflight failed: 0.6.0 does not allow upgrades from 0.5.0.") {
		t.Errorf("upgrade: %+v", r)
	}
	h.assertCleanedUp(t)
}

// ---- Kubernetes on three nodes -----------------------------------------------------

func TestKubernetesUpgradeOnThreeNodes(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", K3s: true, RunID: "42-1-k3s"})
	h.runner.run(context.Background())
	mustPass(t, h)

	res := wantResults(t, h, pass, "Create servers", "SSH", "Install v0.6.0 one k3s patch behind",
		"Join kwerft-e2e-42-1-k3s-w1", "Join kwerft-e2e-42-1-k3s-w2", "3 nodes on k3s v1.37.0+k3s1", "App before the upgrade",
		"Kubernetes v1.37.0+k3s1 → v1.37.1+k3s1 through the console", "App availability during the upgrade", "After the upgrade", "Crash loop alert")
	up := res["Kubernetes v1.37.0+k3s1 → v1.37.1+k3s1 through the console"].Detail
	for _, want := range []string{"→ Running → Verifying → Succeeded",
		"kwerft-e2e-42-1-k3s: Waiting→Upgrading→Done", "kwerft-e2e-42-1-k3s-w1: Waiting→Draining→Upgrading→Done",
		"order kwerft-e2e-42-1-k3s, kwerft-e2e-42-1-k3s-w1, kwerft-e2e-42-1-k3s-w2"} {
		if !strings.Contains(up, want) {
			t.Errorf("upgrade detail lacks %q: %s", want, up)
		}
	}
	// The control plane's restart was a short gap for the App.
	if d := res["App availability during the upgrade"].Detail; strings.Contains(d, "longest gap none") || !strings.Contains(d, "2 of") {
		t.Errorf("availability: %s", d)
	}
	if d := res["After the upgrade"].Detail; !strings.Contains(d, "Ready on v1.37.1+k3s1") {
		t.Errorf("after: %s", d)
	}
	if !strings.Contains(res["Install v0.6.0 one k3s patch behind"].Detail, "KWERFT_K3S_VERSION=v1.37.0+k3s1 (the release pins v1.37.1+k3s1)") {
		t.Errorf("install: %s", res["Install v0.6.0 one k3s patch behind"].Detail)
	}
	// Every node was installed with the override; workers joined with a token, after trusting staging.
	cmds := h.allCommands()
	if n := strings.Count(cmds, "KWERFT_K3S_VERSION='v1.37.0+k3s1' bash '/root/kwerft-install-0.6.0.sh'"); n != 3 {
		t.Errorf("%d installer runs with the override:\n%s", n, cmds)
	}
	if strings.Count(cmds, "'--join' 'https://203.0.113.10.sslip.io' '--token' 'kwft_join_") != 2 || strings.Count(cmds, "update-ca-certificates") != 2 {
		t.Errorf("joins:\n%s", cmds)
	}
	var tokens int
	for _, m := range h.masked {
		if strings.HasPrefix(m, "kwft_join_") {
			tokens++
		}
	}
	if tokens != 2 {
		t.Errorf("join tokens masked: %v", h.masked)
	}
	// Three servers in one network, all gone; the bill counts each.
	h.cloud.mu.Lock()
	for id, nets := range h.cloud.attached {
		t.Errorf("server %d still attached to %v", id, nets)
	}
	h.cloud.mu.Unlock()
	md := h.markdown()
	if !strings.Contains(md, "(3 servers, 3 started server-hour(s)") || !strings.Contains(md, "network ") {
		t.Errorf("summary:\n%s", md)
	}
}

func TestKubernetesGapOverTheLimit(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", K3s: true, MaxGap: time.Nanosecond})
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed with a gap over -max-gap")
	}
	if r := resultsByName(h.runner.rep)["App availability during the upgrade"]; r.Status != fail || !strings.Contains(r.Detail, "more than the") {
		t.Errorf("availability: %+v", r)
	}
	h.assertCleanedUp(t)
}

func TestFailedJoinStillDeletesEverything(t *testing.T) {
	h := newHarnessWith(t, config{Version: "0.6.0", K3s: true, Workers: 1})
	h.console.rejectJoins = true
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed with a worker that did not join")
	}
	if r := resultsByName(h.runner.rep)["Join kwerft-e2e-42-1-fresh-w1"]; r.Status != fail || !strings.Contains(r.Detail, "installer exited 20 (network/DNS)") {
		t.Errorf("join: %+v", r)
	}
	h.assertCleanedUp(t)
}

// ---- backup and restore -------------------------------------------------------------

func restoreHarness(t *testing.T) (*harness, *fakeS3) {
	s3 := newFakeS3(t)
	h := newHarnessWith(t, config{Version: "0.6.0", Restore: true, RunID: "42-1-restore", S3: s3.config()})
	h.console.s3 = s3
	h.runner.bucketHTTP = s3.Client()
	return h, s3
}

func TestBackupRestoredOntoANewServer(t *testing.T) {
	h, s3 := restoreHarness(t)
	s3.put("other-run/velero/backups/x/velero-backup.json") // another run's: never touched
	s3.failFirst["42-1-restore/etcd/on-demand-snapshot"] = 1
	h.runner.run(context.Background())
	mustPass(t, h)

	res := wantResults(t, h, pass, "Install v0.6.0", "Backups configured", "Data to restore", "Cluster backup", "Old server deleted",
		"New server", "Restore v0.6.0 with --restore latest", "Console restored", "Apps restored", "Volume data restored",
		"Secret value restored", "Domain and DNS")
	if d := res["Backups configured"].Detail; !strings.Contains(d, "prefix 42-1-restore, Ready") || !strings.Contains(d, "recovery key kept") {
		t.Errorf("backups: %s", d)
	}
	if d := res["Console restored"].Detail; !strings.Contains(d, "version 0.6.0") || !strings.Contains(d, "same password") {
		t.Errorf("console: %s", d)
	}
	if d := res["Domain and DNS"].Detail; !strings.Contains(d, "kept 203.0.113.10.sslip.io") || !strings.Contains(d, "new server 203.0.113.11") ||
		!strings.Contains(d, "the installer said: DNS         point 203.0.113.10.sslip.io") {
		t.Errorf("dns: %s", d)
	}

	// The restore: on the new server, config and keys uploaded, no --domain.
	b := h.remoteFor("203.0.113.11")
	cmds := strings.Join(b.commands(), "\n")
	want := "bash '/root/kwerft-install-0.6.0.sh' '--version' '0.6.0' '--yes' '--config' '/root/kwerft-e2e/kwerft.yaml' '--restore' 'latest' '--acme-server' 'staging'"
	if !strings.Contains(cmds, want) {
		t.Errorf("restore command, want %s:\n%s", want, cmds)
	}
	if y := b.uploads["/root/kwerft-e2e/kwerft.yaml"]; !strings.Contains(y, "  prefix: 42-1-restore\n") || !strings.Contains(y, "  recoveryKeyFile: /root/kwerft-e2e/recovery.key\n") {
		t.Errorf("kwerft.yaml:\n%s", y)
	}
	key := strings.TrimSpace(b.uploads["/root/kwerft-e2e/recovery.key"])
	if key == "" || key != h.console.recoveryKey || !slices.Contains(h.masked, key) || strings.Contains(h.log.String(), key) {
		t.Errorf("recovery key %q: uploaded, masked (%v) and never logged", key, slices.Contains(h.masked, key))
	}
	for _, c := range b.commands() {
		if strings.Contains(c, key) || strings.Contains(c, s3.creds.SecretKey) {
			t.Errorf("a secret is in a command line: %s", c)
		}
	}
	// The secret value is masked and never in the report.
	for _, m := range h.masked {
		if len(m) == 32 && strings.Contains(h.markdown(), m) {
			t.Error("the secret value is in the report")
		}
	}

	// After the restore, the old names were connected to the new server.
	h.console.mu.Lock()
	dialed := slices.Clone(h.console.dialed)
	h.console.mu.Unlock()
	if !slices.Contains(dialed, "203.0.113.11:443") {
		t.Errorf("never connected to the new server: %v", dialed)
	}

	// The prefix is gone (after a retried deletion), the other run's objects stay.
	if left := s3.keys("42-1-restore/"); len(left) != 0 {
		t.Errorf("left in the bucket: %v", left)
	}
	if len(s3.keys("other-run/")) != 1 {
		t.Error("another run's objects were deleted")
	}
	if !strings.Contains(strings.Join(h.runner.rep.Cleanup, "; "), "bucket kwerft-e2e, prefix 42-1-restore/: 4 object(s) deleted") {
		t.Errorf("cleanup: %v", h.runner.rep.Cleanup)
	}
	if !strings.Contains(h.markdown(), "(2 servers, 2 started server-hour(s)") {
		t.Errorf("cost:\n%s", h.markdown())
	}
}

func TestRestoreFailureStillEmptiesThePrefix(t *testing.T) {
	h, s3 := restoreHarness(t)
	h.remoteFor("203.0.113.11").restoreFail = true
	h.runner.run(context.Background())
	if h.runner.rep.passed() {
		t.Fatal("passed although the restore failed")
	}
	h.assertCleanedUp(t)
	if r := resultsByName(h.runner.rep)["Restore v0.6.0 with --restore latest"]; r.Status != fail || !strings.Contains(r.Detail, "installer exited 60 (restore)") {
		t.Errorf("restore: %+v", r)
	}
	if !strings.Contains(h.runner.rep.Log, "Cannot read the backups") {
		t.Errorf("log: %q", h.runner.rep.Log)
	}
	if left := s3.keys("42-1-restore/"); len(left) != 0 {
		t.Errorf("left in the bucket: %v", left)
	}
}

func TestRestoreRefusesAUsedPrefix(t *testing.T) {
	h, s3 := restoreHarness(t)
	s3.put("42-1-restore/velero/backups/old/velero-backup.json")
	h.runner.run(context.Background())
	if r := resultsByName(h.runner.rep)["Backups configured"]; r.Status != fail || !strings.Contains(r.Detail, "already holds 1 object(s); left alone") {
		t.Errorf("backups: %+v", r)
	}
	h.assertCleanedUp(t)
	if len(s3.keys("42-1-restore/")) != 1 {
		t.Error("objects of a prefix the run did not own were deleted")
	}
}

// ---- pieces ------------------------------------------------------------------

func TestReadEvents(t *testing.T) {
	stream := ": ping\n\nevent: upgrade\ndata: {\"name\":\"u\",\"phase\":\"Running\",\"nodes\":[{\"name\":\"n\",\"state\":\"Draining\"}]}\n\n" +
		"event: upgrade\ndata: {\"name\":\"u\",\"phase\":\"Succeeded\",\"finished\":true}\n\nevent: end\ndata: {\"phase\":\"Succeeded\"}\n\n"
	var seen []string
	end, err := readEvents(strings.NewReader(stream), func(u upgradeView) { seen = append(seen, u.Phase) })
	if err != nil || end.Phase != "Succeeded" || strings.Join(seen, ",") != "Running,Succeeded" {
		t.Errorf("%v %+v %v", err, end, seen)
	}
	end, err = readEvents(strings.NewReader("event: upgrade\ndata: {\"phase\":\"Backup\"}\n\n"), func(upgradeView) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) || end != (streamEnd{}) {
		t.Errorf("a broken stream: %v %+v", err, end)
	}
	end, _ = readEvents(strings.NewReader("event: end\ndata: {\"reason\":\"session\"}\n\n"), func(upgradeView) {})
	if end.Reason != "session" {
		t.Errorf("session end: %+v", end)
	}
	end, _ = readEvents(strings.NewReader("event: gone\ndata: {\"name\":\"u\"}\n\n"), func(upgradeView) {})
	if !end.Gone {
		t.Errorf("gone: %+v", end)
	}
}

func TestProberGaps(t *testing.T) {
	now := time.Unix(0, 0)
	answers := []bool{true, false, false, true, false, true, false}
	p := &prober{url: "https://x/", want: "ok", now: func() time.Time { return now }, started: now, lastOK: now}
	i := 0
	p.get = func(context.Context, string) (int, string, error) {
		ok := answers[i]
		i++
		if ok {
			return 200, "ok", nil
		}
		return 503, "down", nil
	}
	for range answers {
		now = now.Add(time.Second)
		p.probe(context.Background())
	}
	p.cancel, p.done = func() {}, make(chan struct{})
	close(p.done)
	now = now.Add(5 * time.Second) // it never came back: the open gap counts
	res := p.finish()
	// Gaps: 1s→4s (3s), 4s→6s (2s), 6s→12s (6s, open at the end).
	if res.Longest != 6*time.Second || res.Failed != 4 || res.Total != 7 || !strings.Contains(res.LastError, "HTTP 503") {
		t.Errorf("%+v", res)
	}
	if !strings.Contains(res.String(), "longest gap 6s, 4 of 7 request(s) failed") {
		t.Errorf("%s", res)
	}
}

func TestPreviousPatch(t *testing.T) {
	for in, want := range map[string]string{"v1.37.1+k3s1": "v1.37.0+k3s1", "v1.38.12+k3s2": "v1.38.11+k3s2"} {
		if got, err := previousPatch(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	if _, err := previousPatch("v1.38.0+k3s1"); err == nil || !strings.Contains(err.Error(), "-k3s-from") {
		t.Errorf("patch 0: %v", err)
	}
}

// TestProductNames keeps the harness's copies of product names equal.
func TestProductNames(t *testing.T) {
	if annotationFault != upgrades.AnnotationFault || faultInstall != upgrades.FaultInstall {
		t.Error("fault annotation differs from internal/upgrades")
	}
	for _, n := range []string{"kwerft-0.6.0-x7k2p", strings.Repeat("a", 58) + "-b", strings.Repeat("k", 70)} {
		if runnerJobName(n) != controllers.RunnerJobName(n) {
			t.Errorf("%s: %s, product %s", n, runnerJobName(n), controllers.RunnerJobName(n))
		}
	}
	if !strings.HasSuffix(defaultInstallBase, "/main") || upgrades.DefaultInstallBaseURL != defaultInstallBase {
		t.Errorf("install base %s, product %s", defaultInstallBase, upgrades.DefaultInstallBaseURL)
	}
}

func TestScripts(t *testing.T) {
	s := chartValuesScript(defaultChartRef, "0.5.0", []string{"e2e.faults=true"}, []string{"--upgrade-faults"})
	for _, want := range []string{"--reuse-values --set 'e2e.faults=true' --wait", `case "$args" in *'--upgrade-faults'*) ;;`} {
		if !strings.Contains(s, want) {
			t.Errorf("chart values script lacks %q:\n%s", want, s)
		}
	}
	f := faultScript("kwerft-0.6.0-x7k2p", faultInstall)
	for _, want := range []string{"annotate upgrades.kwerft.dev 'kwerft-0.6.0-x7k2p' 'kwerft.dev/e2e-fault=install' --overwrite",
		"get job 'kwerft-0.6.0-x7k2p-runner'", "*'--fault=install'*)", "delete job 'kwerft-0.6.0-x7k2p-runner' --cascade=foreground"} {
		if !strings.Contains(f, want) {
			t.Errorf("fault script lacks %q:\n%s", want, f)
		}
	}
	if tr := trustScript(defaultStagingRoots); !strings.Contains(tr, "kwerft-e2e-staging-2.crt 'https://letsencrypt.org/certs/staging/letsencrypt-stg-root-x2.pem'") {
		t.Errorf("trust script:\n%s", tr)
	}
	if uploadCmd("/root/kwerft-e2e/s3.secret") != "umask 077 && mkdir -p '/root/kwerft-e2e/' && cat >'/root/kwerft-e2e/s3.secret'" {
		t.Errorf("upload: %s", uploadCmd("/root/kwerft-e2e/s3.secret"))
	}
}

func TestPinHost(t *testing.T) {
	var asked []string
	tr := &http.Transport{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		asked = append(asked, addr)
		return nil, errors.New("no network in tests")
	}}
	c := pinHost(&http.Client{Transport: tr}, "203.0.113.10.sslip.io", "203.0.113.11")
	for _, u := range []string{"https://203.0.113.10.sslip.io/", "https://files.203.0.113.10.sslip.io/x", "https://example.com/"} {
		_, _ = c.Get(u)
	}
	if strings.Join(asked, ",") != "203.0.113.11:443,203.0.113.11:443,example.com:443" {
		t.Errorf("dialled %v", asked)
	}
	if tr.DialContext == nil || c.Transport == http.RoundTripper(tr) {
		t.Error("the original transport was changed")
	}
}

func TestBucketDeletePrefix(t *testing.T) {
	s3 := newFakeS3(t)
	for _, k := range []string{"run/a", "run/b/c", "run/d", "run2/x", "other"} {
		s3.put(k)
	}
	b := newBucket(s3.config(), s3.Client())
	n, err := b.deletePrefix(context.Background(), "run/")
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	if got := strings.Join(s3.keys(""), ","); got != "other,run2/x" {
		t.Errorf("left %s", got)
	}
	if _, err := b.deletePrefix(context.Background(), "/"); err == nil {
		t.Error("deleted the whole bucket")
	}
	// Wrong keys: the storage refuses the signature.
	bad := s3.config()
	bad.SecretKey = "wrong"
	if _, err := newBucket(bad, s3.Client()).list(context.Background(), "run2/"); err == nil || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("bad key: %v", err)
	}
}

func TestSweepBucket(t *testing.T) {
	s3 := newFakeS3(t)
	s3.put("7-1-restore/velero/x")
	s3.put("7-1-restore/etcd/y")
	s3.put("8-1-restore/velero/x")
	res, err := sweepBucket(context.Background(), s3.config(), s3.Client(), "7-1-restore", true, sweepResult{}, nil)
	if err != nil || len(res.Deleted) != 1 || !strings.Contains(res.Deleted[0], "would delete 2 object(s) under kwerft-e2e/7-1-restore/") || len(s3.keys("")) != 3 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	res, err = sweepBucket(context.Background(), s3.config(), s3.Client(), "7-1-restore", false, sweepResult{}, nil)
	if err != nil || len(res.Deleted) != 1 || strings.Join(s3.keys(""), ",") != "8-1-restore/velero/x" {
		t.Fatalf("sweep: %+v %v %v", res, err, s3.keys(""))
	}
	if _, err := sweepBucket(context.Background(), s3Config{}, nil, "7-1-restore", false, sweepResult{}, nil); err == nil {
		t.Error("no bucket configured is not an error")
	}
}

func TestSweepDeletesNetworks(t *testing.T) {
	f := newFakeCloud(t)
	c := f.client()
	old := time.Now().Add(-4 * time.Hour)
	f.mu.Lock()
	f.networks[1] = &hNetwork{ID: 1, Name: "kwerft-e2e-old", Created: old, Labels: map[string]string{labelE2E: "true", labelRun: "1-1-k3s"}}
	f.networks[2] = &hNetwork{ID: 2, Name: "prod", Created: old, Labels: map[string]string{}}
	f.mu.Unlock()
	res, err := sweep(context.Background(), c, time.Now(), 3*time.Hour, "", false)
	if err != nil || len(res.Deleted) != 1 || !strings.Contains(res.Deleted[0], "network kwerft-e2e-old") {
		t.Fatalf("%+v %v", res, err)
	}
	if f.networkCount() != 1 {
		t.Error("the foreign network was deleted")
	}
}

func TestModeConfig(t *testing.T) {
	base := config{Version: "0.6.0", RunID: "1-1-x", ServerTypes: []string{"cx33"}, Locations: []string{"nbg1"}, Images: []string{"ubuntu-26.04"}}
	for name, tc := range map[string]struct {
		mod  func(*config)
		want string
	}{
		"console":         {func(c *config) { c.From, c.ViaConsole = "0.5.0", true }, ""},
		"console no from": {func(c *config) { c.ViaConsole = true }, "-from"},
		"fault no from":   {func(c *config) { c.Fault = true }, "-from"},
		"k3s":             {func(c *config) { c.K3s, c.Workers = true, 2 }, ""},
		"k3s with from":   {func(c *config) { c.K3s, c.Workers, c.From = true, 2, "0.5.0" }, "does not combine"},
		"k3s workers":     {func(c *config) { c.K3s, c.Workers = true, 0 }, "-workers"},
		"k3s bad version": {func(c *config) { c.K3s, c.Workers, c.K3sFrom = true, 2, "1.37.0" }, "-k3s-from"},
		"k3s-to alone":    {func(c *config) { c.K3sTo = "v1.37.1+k3s1" }, "need -k3s"},
		"restore": {func(c *config) {
			c.Restore, c.S3 = true, s3Config{Endpoint: "https://fsn1.your-objectstorage.com", Bucket: "b"}
		}, ""},
		"restore no s3":    {func(c *config) { c.Restore = true }, "-s3-endpoint"},
		"restore http":     {func(c *config) { c.Restore, c.S3 = true, s3Config{Endpoint: "http://x", Bucket: "b"} }, "-s3-endpoint"},
		"restore and k3s":  {func(c *config) { c.Restore, c.K3s, c.Workers = true, true, 2 }, "does not combine"},
		"restore and from": {func(c *config) { c.Restore, c.From, c.S3 = true, "0.5.0", s3Config{Endpoint: "https://x", Bucket: "b"} }, "does not combine"},
		"long run id k3s":  {func(c *config) { c.K3s, c.Workers, c.RunID = true, 2, strings.Repeat("a", 52) }, "too long"},
	} {
		c := base
		tc.mod(&c)
		err := c.validate()
		if (tc.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

func TestDryRunPlansOfEachMode(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "")
	t.Setenv("E2E_S3_ENDPOINT", "https://fsn1.your-objectstorage.com")
	t.Setenv("E2E_S3_BUCKET", "kwerft-e2e")
	for args, wants := range map[string][]string{
		"-from 0.5.0 -via-console": {"Upgrade from v0.5.0 to v0.6.0 through the console", "POST /api/v1/upgrades {component: Kwerft, version: 0.6.0}", "The Upgrade ends Succeeded", "crash-looping alert", "Project e2e:"},
		"-from 0.5.0 -fault":       {"with a failure injected after the Kwerft stage", "--reuse-values --set e2e.faults=true", "kwerft.dev/e2e-fault=install", "ends RolledBack"},
		"-k3s":                     {"Kubernetes upgrade through the console on 3 nodes", "Cloud Network", "KWERFT_K3S_VERSION=one patch behind", "--join https://<console>", "delete the servers, then the network and the SSH key"},
		"-k3s -k3s-from v1.36.4+k3s1 -k3s-to v1.37.1+k3s1 -workers 1": {"on 2 nodes", "k3s v1.36.4+k3s1 → v1.37.1+k3s1", "On each of the 1 workers"},
		"-restore": {"Backup of v0.6.0 restored onto a new server", "prefix 1-1-x/ must be empty", "--restore latest", "the objects under 1-1-x/ in the bucket"},
	} {
		var out bytes.Buffer
		a := append([]string{"-version", "0.6.0", "-run-id", "1-1-x", "-dry-run"}, strings.Fields(args)...)
		if err := cmdRun(context.Background(), a, &out); err != nil {
			t.Errorf("%s: %v", args, err)
			continue
		}
		for _, w := range wants {
			if !strings.Contains(out.String(), w) {
				t.Errorf("%s: plan lacks %q:\n%s", args, w, out.String())
			}
		}
	}
	// -fault and -restore are usage errors without what they need.
	t.Setenv("E2E_S3_ENDPOINT", "")
	for _, args := range [][]string{{"-fault"}, {"-restore"}, {"-k3s", "-from", "0.5.0"}} {
		err := cmdRun(context.Background(), append([]string{"-version", "0.6.0", "-dry-run"}, args...), io.Discard)
		var u usageError
		if !errors.As(err, &u) {
			t.Errorf("%v: %v", args, err)
		}
	}
}
