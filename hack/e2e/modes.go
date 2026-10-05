package main

// The runs beyond a fresh install and an installer re-run: an upgrade
// through the console (also with a forced failure), a Kubernetes upgrade on
// three nodes, and a backup restored onto a new server.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Mirrors of the product's names (internal/upgrades, internal/controllers);
// TestProductNames keeps them equal.
const (
	annotationFault = "kwerft.dev/e2e-fault"
	faultInstall    = "install"
)

// runnerJobName is controllers.RunnerJobName: the Upgrade's runner Job.
func runnerJobName(upgrade string) string {
	name := upgrade + "-runner"
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

// ---- Kwerft through the console -------------------------------------------------

func (r *runner) executeViaConsole(ctx context.Context) error {
	if err := r.step("Create server", func() (string, error) { return r.createPrimary(ctx) }); err != nil {
		return err
	}
	if err := r.step("SSH", func() (string, error) { return r.connect(ctx, r.cur) }); err != nil {
		return err
	}
	if err := r.install(ctx, r.cfg.From, "Install v"+r.cfg.From); err != nil {
		return err
	}
	if err := r.firstSignIn(ctx, r.cfg.From); err != nil {
		return err
	}
	if err := r.step("App before the upgrade", func() (string, error) { return r.seedApp(ctx, 1) }); err != nil {
		return err
	}
	if err := r.chartValues(ctx); err != nil {
		return err
	}
	before, err := r.console.appPods(ctx, "e2e-before", "hello")
	if err != nil {
		return r.step("App's pods before the upgrade", func() (string, error) { return "", err })
	}

	name, want := "Upgrade to v"+r.cfg.Version+" through the console", "Succeeded"
	if r.cfg.Fault {
		name, want = "Upgrade to v"+r.cfg.Version+" through the console, failing after the Kwerft stage", "RolledBack"
	}
	probe := r.startProbe(ctx, r.helloURL(), "Hostname")
	upErr := r.step(name, func() (string, error) {
		detail, _, err := r.upgradeThroughConsole(ctx, upgradeRequest{Component: "Kwerft", Version: r.cfg.Version}, want, r.cfg.Fault, nil)
		return detail, err
	})
	r.availability("App availability during the upgrade", probe.finish())
	if upErr != nil {
		return upErr
	}
	if r.cfg.Fault {
		return r.step("After the rollback", func() (string, error) { return r.afterRollback(ctx, before) })
	}
	if err := r.step("After the upgrade", func() (string, error) { return r.afterUpgrade(ctx) }); err != nil {
		return err
	}
	return r.suite(ctx)
}

// chartValues sets what the run needs on the installed release's chart:
// e2e.faults for the forced failure (the runner honours its annotation only
// then), and the install repository when the run's installers come from
// another one than the console's default.
func (r *runner) chartValues(ctx context.Context) error {
	var sets, flags []string
	if r.cfg.Fault {
		sets, flags = append(sets, "e2e.faults=true"), append(flags, "--upgrade-faults")
	}
	if b := r.cfg.installBase(); b != "" && b != defaultInstallBase {
		sets, flags = append(sets, "upgrades.installBaseURL="+b), append(flags, "--install-base-url="+b)
	}
	if len(sets) == 0 {
		return nil
	}
	return r.step("Chart values for this run", func() (string, error) {
		out, code, err := r.sh(ctx, chartValuesScript(r.cfg.ChartRef, r.cfg.From, sets, flags))
		if err != nil || code != 0 {
			return "", fmt.Errorf("exit %d %v: %s", code, err, lastLines(out, 5))
		}
		// The console was restarted with the new flags.
		if err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
			err := r.console.healthz(ctx)
			return err == nil, err
		}); err != nil {
			return "", err
		}
		if err := r.console.signIn(ctx, r.owner); err != nil {
			return "", fmt.Errorf("sign in again: %w", err)
		}
		return strings.Join(sets, ", ") + "; the console runs with " + strings.Join(flags, " "), nil
	})
}

// chartValuesScript changes values of the installed release (the
// installer's next run resets them) and checks the console's flags.
func chartValuesScript(chart, version string, sets, flags []string) string {
	var b strings.Builder
	b.WriteString("set -eu\nexport KUBECONFIG=/etc/rancher/k3s/k3s.yaml HOME=/root\n")
	b.WriteString("helm upgrade kwerft " + shellQuote(chart) + " --version " + shellQuote(version) + " --namespace kwerft-system --reuse-values")
	for _, s := range sets {
		b.WriteString(" --set " + shellQuote(s))
	}
	b.WriteString(" --wait --timeout 10m >/dev/null\n")
	b.WriteString("args=$(k3s kubectl -n kwerft-system get deployment kwerft -o jsonpath='{.spec.template.spec.containers[0].args}')\n")
	for _, f := range flags {
		fmt.Fprintf(&b, "case \"$args\" in *%s*) ;; *) echo %s >&2; exit 1 ;; esac\n",
			shellQuote(f), shellQuote("the console runs without "+f+": the chart of "+version+" does not have this value"))
	}
	return b.String()
}

// faultScript annotates an Upgrade with the e2e fault and makes sure its
// runner Job carries it: the controller passes the annotation on when it
// creates the Job, which can happen before the annotation lands; such a
// Job is deleted and the controller creates it again (the runner resumes
// from the status).
func faultScript(upgrade, fault string) string {
	job := runnerJobName(upgrade)
	return `set -eu
kc() { k3s kubectl "$@"; }
kc annotate upgrades.kwerft.dev ` + shellQuote(upgrade) + ` ` + shellQuote(annotationFault+"="+fault) + ` --overwrite >/dev/null
for _ in $(seq 1 150); do
  args=$(kc -n kwerft-system get job ` + shellQuote(job) + ` -o jsonpath='{.spec.template.spec.containers[0].args}' 2>/dev/null || true)
  case "$args" in
    *` + shellQuote("--fault="+fault) + `*) echo "runner ` + job + ` carries --fault=` + fault + `"; exit 0 ;;
    "") ;;
    *) kc -n kwerft-system delete job ` + shellQuote(job) + ` --cascade=foreground --wait=true >/dev/null
       echo "deleted runner ` + job + `, created before the annotation" ;;
  esac
  sleep 2
done
echo "runner ` + job + ` never carried --fault=` + fault + `" >&2
exit 1`
}

// upgradeThroughConsole starts an upgrade as the owner and follows its
// events until it finished; it must end in phase want. track sees every
// state.
func (r *runner) upgradeThroughConsole(ctx context.Context, req upgradeRequest, want string, fault bool, track func(*upgradeView)) (string, *upgradeView, error) {
	req.Cluster, req.Password = "local", r.owner.Password
	if req.Component == "Kubernetes" {
		req.ConfirmVersion = req.Version // asked for minors only; harmless for a patch
	}
	var up *upgradeView
	err := r.waitFor(ctx, 5*time.Minute, func(ctx context.Context) (bool, error) {
		u, err := r.console.startUpgrade(ctx, req)
		var he *httpError
		switch {
		case err == nil:
			up = u
			return true, nil
		case errors.As(err, &he) && he.Status == http.StatusConflict && strings.Contains(he.Raw, `"upgrade"`):
			// An earlier attempt whose answer got lost created it.
			var existing struct {
				Upgrade upgradeView `json:"upgrade"`
			}
			if jsonUnmarshal(he.Raw, &existing) == nil && existing.Upgrade.Version == req.Version && existing.Upgrade.Name != "" {
				up = &existing.Upgrade
				return true, nil
			}
			return true, fmt.Errorf("the console refused: %s", he.message())
		case errors.As(err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != http.StatusTooManyRequests:
			return true, fmt.Errorf("the console refused (HTTP %d): %s", he.Status, he.message())
		}
		return false, err // unavailable for a moment: again
	})
	if err != nil {
		return "", nil, err
	}
	r.logf("  upgrade %s created", up.Name)
	start := r.now()
	var notes []string
	if fault {
		out, code, err := r.sh(ctx, faultScript(up.Name, faultInstall))
		if err != nil || code != 0 {
			return up.Name, up, fmt.Errorf("inject the fault: exit %d %v: %s", code, err, lastLines(out, 3))
		}
		notes = append(notes, lastLines(out, 2))
	}
	var phases []string
	final, reconnects, err := r.followUpgrade(ctx, up.Name, 100*time.Minute, func(u *upgradeView) {
		if len(phases) == 0 || phases[len(phases)-1] != u.Phase {
			phases = append(phases, u.Phase)
			r.logf("  upgrade %s: %s %s", u.Name, u.Phase, u.Message)
		}
		if track != nil {
			track(u)
		}
	})
	detail := fmt.Sprintf("%s: %s in %s", up.Name, strings.Join(phases, " → "), fmtDuration(r.now().Sub(start)))
	if final != nil && req.Component == "Kwerft" {
		done := 0
		for _, s := range final.Steps {
			if s.State == "Done" || s.State == "Skipped" {
				done++
			}
		}
		detail += fmt.Sprintf("; %d of %d installer stages done", done, len(final.Steps))
	}
	if reconnects > 0 {
		detail += fmt.Sprintf("; the event stream was reconnected %d time(s) (the console restarts)", reconnects)
	}
	for _, n := range notes {
		detail += "; " + strings.ReplaceAll(n, "\n", "; ")
	}
	if err != nil {
		return detail, final, err
	}
	if final.Phase != want {
		if log, lerr := r.console.upgradeLog(ctx, up.Name); lerr == nil && log != "" {
			r.rep.Log = lastLines(log, 60)
		}
		return detail, final, fmt.Errorf("ended %s, want %s: %s", final.Phase, want, strings.TrimSpace(final.Reason+" "+final.Message))
	}
	if final.Phase != "Succeeded" && final.Message != "" {
		detail += "; " + strings.TrimSpace(final.Reason+": "+final.Message)
	}
	return detail, final, nil
}

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// followUpgrade reads the Upgrade's events until it finished, through the
// console's own restart: a broken stream is opened again (the first event
// is the current state), an ended session signs in again.
func (r *runner) followUpgrade(ctx context.Context, name string, timeout time.Duration, fn func(*upgradeView)) (*upgradeView, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var last *upgradeView
	reconnects := 0
	for {
		end, err := r.console.upgradeEvents(ctx, name, func(u upgradeView) {
			last = &u
			fn(&u)
		})
		switch {
		case end.Gone:
			return last, reconnects, fmt.Errorf("upgrade %s was deleted", name)
		case end.Phase != "":
			final, gerr := r.console.upgrade(ctx, name)
			if gerr != nil {
				if last == nil {
					return nil, reconnects, gerr
				}
				last.Phase = end.Phase
				return last, reconnects, nil
			}
			fn(final)
			return final, reconnects, nil
		case end.Reason == "session":
			if serr := r.console.signIn(ctx, r.owner); serr != nil {
				r.logf("  sign in again: %v", serr)
			}
			continue
		}
		if ctx.Err() != nil {
			phase := "unknown"
			if last != nil {
				phase = last.Phase
			}
			return last, reconnects, fmt.Errorf("gave up after %s waiting for upgrade %s (last phase %s): %v", fmtDuration(timeout), name, phase, err)
		}
		var he *httpError
		if errors.As(err, &he) && he.Status == http.StatusUnauthorized {
			_ = r.console.signIn(ctx, r.owner)
		}
		reconnects++
		r.logf("  event stream: %v; reconnecting", err)
		if serr := sleepCtx(ctx, r.cfg.Poll); serr != nil {
			continue // the deadline check above reports it
		}
	}
}

// availability records what the prober saw as its own result.
func (r *runner) availability(name string, res probeResult) {
	out := result{Name: name, Status: pass, Duration: res.Duration, Detail: res.String()}
	if r.cfg.MaxGap > 0 && res.Longest > r.cfg.MaxGap {
		out.Status = fail
		out.Detail += fmt.Sprintf(" — more than the %s allowed (-max-gap)", fmtDuration(r.cfg.MaxGap))
	} else if res.Longest > 30*time.Second {
		r.rep.note("%s: the App was away for %s at most.", name, fmtGap(res.Longest))
	}
	r.logf("%s %s: %s", map[bool]string{true: "✓", false: "✗"}[out.Status == pass], name, out.Detail)
	r.rep.add(out)
}

// afterRollback: the console is back on the release before, and the App
// was never touched (same pods, same restarts).
func (r *runner) afterRollback(ctx context.Context, before []pod) (string, error) {
	if err := r.waitVersion(ctx, r.cfg.From); err != nil {
		return "", err
	}
	if err := r.console.signIn(ctx, r.owner); err != nil {
		return "", fmt.Errorf("the owner cannot sign in: %w", err)
	}
	if err := r.helloRuns(ctx); err != nil {
		return "", err
	}
	after, err := r.console.appPods(ctx, "e2e-before", "hello")
	if err != nil {
		return "", err
	}
	if !samePods(before, after) {
		return "", fmt.Errorf("the app's pods changed during the failed upgrade: before %s, after %s", podList(before), podList(after))
	}
	return fmt.Sprintf("version %s again, owner signs in, hello.%s answers from the same pod(s) %s", r.cfg.From, r.domain(), podList(after)), nil
}

func samePods(a, b []pod) bool {
	key := func(ps []pod) []string {
		var out []string
		for _, p := range ps {
			out = append(out, p.Name+"/"+strconv.Itoa(int(p.Restarts)))
		}
		slices.Sort(out)
		return out
	}
	return len(a) > 0 && slices.Equal(key(a), key(b))
}

func podList(ps []pod) string {
	var out []string
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s (%d restarts)", p.Name, p.Restarts))
	}
	return "[" + strings.Join(out, ", ") + "]"
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// ---- Kubernetes on three nodes ----------------------------------------------------

func (r *runner) executeK3s(ctx context.Context) error {
	locations := sameZone(r.cfg.Locations)
	if err := r.step("Create servers", func() (string, error) { return r.createCluster(ctx, locations) }); err != nil {
		return err
	}
	if err := r.step("SSH", func() (string, error) {
		var out string
		for _, m := range r.machines {
			os, err := r.connect(ctx, m)
			if err != nil {
				return "", fmt.Errorf("%s: %w", m.name, err)
			}
			out = os
		}
		return fmt.Sprintf("%d servers, %s", len(r.machines), out), nil
	}); err != nil {
		return err
	}
	var from, to string
	if err := r.step("Install v"+r.cfg.Version+" one k3s patch behind", func() (string, error) {
		var pin string
		var err error
		from, to, pin, err = r.k3sVersions(ctx)
		if err != nil {
			return "", err
		}
		detail, err := r.runInstaller(ctx, r.cfg.Version, installOpts{env: []string{"KWERFT_K3S_VERSION=" + from}})
		return fmt.Sprintf("KWERFT_K3S_VERSION=%s (the release pins %s); %s", from, pin, detail), err
	}); err != nil {
		return err
	}
	if err := r.firstSignIn(ctx, r.cfg.Version); err != nil {
		return err
	}
	for _, m := range r.machines[1:] {
		if err := r.step("Join "+m.name, func() (string, error) { return r.joinWorker(ctx, m, from) }); err != nil {
			return err
		}
	}
	if err := r.step(fmt.Sprintf("%d nodes on k3s %s", len(r.machines), from), func() (string, error) {
		return r.waitNodes(ctx, len(r.machines), from, 10*time.Minute)
	}); err != nil {
		return err
	}
	if err := r.step("App before the upgrade", func() (string, error) { return r.seedApp(ctx, 2) }); err != nil {
		return err
	}
	tracker := newNodeTracker()
	probe := r.startProbe(ctx, r.helloURL(), "Hostname")
	upErr := r.step(fmt.Sprintf("Kubernetes %s → %s through the console", from, to), func() (string, error) {
		detail, final, err := r.upgradeThroughConsole(ctx, upgradeRequest{Component: "Kubernetes", Version: to}, "Succeeded", false, tracker.see)
		if final != nil {
			nodes, terr := tracker.check(final, r.cur.name, len(r.machines))
			detail += "; " + nodes
			if err == nil && terr != nil {
				err = terr
			}
			for _, n := range tracker.notes {
				r.rep.note("%s", n)
			}
		}
		return detail, err
	})
	r.availability("App availability during the upgrade", probe.finish())
	if upErr != nil {
		return upErr
	}
	if err := r.step("After the upgrade", func() (string, error) {
		detail, err := r.waitNodes(ctx, len(r.machines), to, 10*time.Minute)
		if err != nil {
			return "", err
		}
		if err := r.console.signIn(ctx, r.owner); err != nil {
			return "", fmt.Errorf("the owner cannot sign in: %w", err)
		}
		if err := r.helloRuns(ctx); err != nil {
			return "", err
		}
		return detail + "; owner signs in, hello." + r.domain() + " answers and runs", nil
	}); err != nil {
		return err
	}
	return r.suite(ctx)
}

// createCluster creates the network and the servers: the control plane,
// then the workers, preferably where the control plane is.
func (r *runner) createCluster(ctx context.Context, locations []string) (string, error) {
	n, err := r.cloud.createNetwork(ctx, r.cfg.serverName(), networkZone(locations[0]), r.labels())
	if err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}
	r.networkID = n.ID
	m, err := r.createMachine(ctx, r.cfg.serverName(), locations)
	if m != nil {
		r.cur = m
	}
	if err != nil {
		return r.rep.Server, err
	}
	for i := 1; i <= r.cfg.Workers; i++ {
		if _, err := r.createMachine(ctx, r.cfg.workerName(i), prefer(m.location, locations)); err != nil {
			return r.rep.Server, err
		}
	}
	return fmt.Sprintf("network %d (10.0.0.0/16); %s", n.ID, r.rep.Server), nil
}

// k3sVersions are the versions of the Kubernetes run: from the flags, else
// one patch behind the installer's K3S_VERSION and the pin itself.
func (r *runner) k3sVersions(ctx context.Context) (from, to, pin string, err error) {
	if err := r.downloadInstaller(ctx, r.cur, r.cfg.Version); err != nil {
		return "", "", "", err
	}
	out, code, err := r.sh(ctx, `sed -n 's/^K3S_VERSION="\([^"]*\)".*/\1/p' `+shellQuote(installerPath(r.cfg.Version)))
	if err != nil || code != 0 || !k3sVersionRE.MatchString(out) {
		return "", "", "", fmt.Errorf("read K3S_VERSION from the installer: exit %d %v %q", code, err, out)
	}
	pin = out
	from, to = r.cfg.K3sFrom, cmpOr(r.cfg.K3sTo, pin)
	if from == "" {
		if from, err = previousPatch(pin); err != nil {
			return "", "", pin, err
		}
	}
	if from == to {
		return "", "", pin, fmt.Errorf("k3s %s is already the target", from)
	}
	return from, to, pin, nil
}

// previousPatch is one patch release before a k3s version: v1.37.1+k3s1 →
// v1.37.0+k3s1.
func previousPatch(v string) (string, error) {
	core, build, _ := strings.Cut(strings.TrimPrefix(v, "v"), "+")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%s is not a k3s version", v)
	}
	patch, err := strconv.Atoi(parts[2])
	if err != nil || patch == 0 {
		return "", fmt.Errorf("k3s %s has no patch release before it in its minor: give -k3s-from", v)
	}
	return fmt.Sprintf("v%s.%s.%d+%s", parts[0], parts[1], patch-1, cmpOr(build, "k3s1")), nil
}

// joinWorker joins m to the cluster as a worker with a join token from the
// console, the way Nodes › Add node shows it.
func (r *runner) joinWorker(ctx context.Context, m *machine, k3s string) (string, error) {
	if out, code, err := r.shOn(ctx, m, trustScript(r.cfg.StagingRoots)); err != nil || code != 0 {
		return "", fmt.Errorf("trust Let's Encrypt's staging roots: exit %d %v %s", code, err, lastLines(out, 3))
	}
	token, err := r.console.joinToken(ctx, "local", "worker")
	if err != nil {
		return "", fmt.Errorf("join command: %w", err)
	}
	r.mask(token)
	detail, err := r.runInstaller(ctx, r.cfg.Version, installOpts{on: m, env: []string{"KWERFT_K3S_VERSION=" + k3s},
		join: []string{"--join", "https://" + r.domain(), "--token", token, "--role", "worker"}})
	return "worker, " + detail, err
}

// trustScript adds certificates to the system's trust store (curl's).
func trustScript(urls []string) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for i, u := range urls {
		fmt.Fprintf(&b, "curl -fsSL --retry 5 -o /usr/local/share/ca-certificates/kwerft-e2e-staging-%d.crt %s\n", i+1, shellQuote(u))
	}
	b.WriteString("update-ca-certificates >/dev/null\n")
	return b.String()
}

// waitNodes waits for want nodes, all Ready and schedulable on version.
func (r *runner) waitNodes(ctx context.Context, want int, version string, timeout time.Duration) (string, error) {
	var nodes []nodeView
	err := r.waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		var err error
		if nodes, err = r.console.nodes(ctx, "local"); err != nil {
			return false, err
		}
		if len(nodes) != want {
			return false, fmt.Errorf("%d node(s), want %d", len(nodes), want)
		}
		for _, n := range nodes {
			switch {
			case !n.Ready:
				return false, fmt.Errorf("node %s is %s", n.Name, cmpOr(n.Status, "not ready"))
			case n.Kubelet != version:
				return false, fmt.Errorf("node %s runs %s, want %s", n.Name, n.Kubelet, version)
			case n.Unschedulable:
				return false, fmt.Errorf("node %s is cordoned", n.Name)
			}
		}
		return true, nil
	})
	var names []string
	for _, n := range nodes {
		names = append(names, fmt.Sprintf("%s (%s)", n.Name, strings.Join(n.Roles, ",")))
	}
	return fmt.Sprintf("%s, Ready on %s", strings.Join(names, ", "), version), err
}

// nodeTracker follows a Kubernetes upgrade's per-node progress.
type nodeTracker struct {
	states  map[string][]string // each node's states, in order
	order   []string            // nodes in the order they left Waiting
	maxBusy int                 // nodes draining or upgrading at once, at most
	busyAt  string
	notes   []string
}

func newNodeTracker() *nodeTracker { return &nodeTracker{states: map[string][]string{}} }

func (t *nodeTracker) see(u *upgradeView) {
	var busy []string
	for _, n := range u.Nodes {
		seq := t.states[n.Name]
		if len(seq) == 0 || seq[len(seq)-1] != n.State {
			t.states[n.Name] = append(seq, n.State)
		}
		if n.State != "Waiting" && !slices.Contains(t.order, n.Name) {
			t.order = append(t.order, n.Name)
		}
		if n.State == "Draining" || n.State == "Upgrading" {
			busy = append(busy, n.Name)
		}
	}
	if len(busy) > t.maxBusy {
		t.maxBusy, t.busyAt = len(busy), strings.Join(busy, ", ")
	}
}

// check: every node reached Done, the control plane first.
func (t *nodeTracker) check(final *upgradeView, controlPlane string, want int) (string, error) {
	var parts []string
	for _, n := range final.Nodes {
		parts = append(parts, n.Name+": "+strings.Join(t.states[n.Name], "→"))
	}
	detail := "nodes " + strings.Join(parts, "; ")
	if t.maxBusy > 1 {
		t.notes = append(t.notes, fmt.Sprintf("Kubernetes upgrade: %d nodes were draining or upgrading at the same time (%s); SUC's concurrency is 1, so this is a node whose kubelet had not reported the target yet.", t.maxBusy, t.busyAt))
	}
	if len(final.Nodes) != want {
		return detail, fmt.Errorf("the upgrade shows %d node(s), want %d", len(final.Nodes), want)
	}
	for _, n := range final.Nodes {
		if n.State != "Done" {
			return detail, fmt.Errorf("node %s ended %s: %s", n.Name, n.State, n.Message)
		}
	}
	if len(t.order) > 0 && t.order[0] != controlPlane {
		return detail, fmt.Errorf("%s was upgraded before the control plane %s (order %s)", t.order[0], controlPlane, strings.Join(t.order, ", "))
	}
	return detail + "; order " + strings.Join(t.order, ", "), nil
}

// ---- backup and restore --------------------------------------------------------------

const restoreProject = "e2e-restore"

func (r *runner) executeRestore(ctx context.Context) error {
	if err := r.step("Create server", func() (string, error) { return r.createPrimary(ctx) }); err != nil {
		return err
	}
	if err := r.step("SSH", func() (string, error) { return r.connect(ctx, r.cur) }); err != nil {
		return err
	}
	if err := r.install(ctx, r.cfg.Version, "Install v"+r.cfg.Version); err != nil {
		return err
	}
	if err := r.firstSignIn(ctx, r.cfg.Version); err != nil {
		return err
	}
	if err := r.step("Backups configured", func() (string, error) { return r.configureBackups(ctx) }); err != nil {
		return err
	}
	marker, secret := "kwerft-e2e-volume-"+r.cfg.RunID+"-"+randomString(8), randomString(32)
	r.mask(secret)
	if err := r.step("Data to restore", func() (string, error) { return r.seedRestoreData(ctx, marker, secret) }); err != nil {
		return err
	}
	if err := r.step("Cluster backup", func() (string, error) { return r.backupNow(ctx) }); err != nil {
		return err
	}

	old := r.cur
	r.host = r.domain() // the backup keeps the old server's names
	if err := r.step("Old server deleted", func() (string, error) {
		if err := r.deleteMachine(ctx, old); err != nil {
			return "", err
		}
		return fmt.Sprintf("server %d (%s) is gone; the bucket is all that is left", old.id, old.ip), nil
	}); err != nil {
		return err
	}
	r.console = nil
	if err := r.step("New server", func() (string, error) {
		m, err := r.createMachine(ctx, r.cfg.restoredName(), prefer(old.location, r.cfg.Locations))
		if m != nil {
			r.cur = m
		}
		if err != nil {
			return "", err
		}
		os, err := r.connect(ctx, m)
		return fmt.Sprintf("%s, %s, %s", m.name, m.ip, os), err
	}); err != nil {
		return err
	}
	if err := r.step("Restore v"+r.cfg.Version+" with --restore latest", func() (string, error) {
		cfgFile, err := r.uploadRestoreConfig(ctx)
		if err != nil {
			return "", err
		}
		return r.runInstaller(ctx, r.cfg.Version, installOpts{config: cfgFile, restore: "latest"})
	}); err != nil {
		return err
	}

	r.pinIP = r.cur.ip
	r.openConsole()
	if err := r.step("Console restored", func() (string, error) {
		detail, err := r.consoleUp(ctx, r.cfg.Version)
		if err != nil {
			return detail, err
		}
		if done, err := r.console.setupComplete(ctx); err != nil || !done {
			return detail, fmt.Errorf("the restored console asks for setup (%v): the owner did not come back", err)
		}
		if err := r.console.signIn(ctx, r.owner); err != nil {
			return detail, fmt.Errorf("the owner cannot sign in with the same password: %w", err)
		}
		return detail + "; setup complete, the owner signs in with the same password", nil
	}); err != nil {
		return err
	}
	// The remaining checks are independent: each reports.
	failed := false
	for _, c := range []struct {
		name string
		fn   func() (string, error)
	}{
		{"Apps restored", func() (string, error) { return r.appsRunning(ctx, restoreProject, "files", "secret") }},
		{"Volume data restored", func() (string, error) {
			u := "https://files." + r.domain() + "/marker.txt"
			if err := r.waitHTTPS(ctx, u, marker, 10*time.Minute); err != nil {
				return "", err
			}
			return u + " answers the marker the Task wrote before the backup", nil
		}},
		{"Secret value restored", func() (string, error) { return r.secretRestored(ctx, secret) }},
		{"Domain and DNS", func() (string, error) { return r.dnsReport(old), nil }},
	} {
		if err := r.step(c.name, c.fn); err != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("restore checks failed")
	}
	return nil
}

// configureBackups saves the run's bucket in Settings › Backups and keeps
// the recovery key from the answer.
func (r *runner) configureBackups(ctx context.Context) (string, error) {
	prefix := r.cfg.RunID
	b := newBucket(r.cfg.S3, r.bucketHTTP)
	keys, err := b.list(ctx, prefix+"/")
	if err != nil {
		return "", fmt.Errorf("the bucket %s: %w", r.cfg.S3.Bucket, err)
	}
	if len(keys) > 0 {
		return "", fmt.Errorf("prefix %s/ in bucket %s already holds %d object(s); left alone", prefix, r.cfg.S3.Bucket, len(keys))
	}
	r.bucket = b // from here on, cleanup empties the prefix
	var key string
	var planCreated bool
	err = r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		var err error
		key, planCreated, err = r.console.saveBackupTarget(ctx, backupTarget{Endpoint: r.cfg.S3.Endpoint, Region: r.cfg.S3.Region,
			Bucket: r.cfg.S3.Bucket, Prefix: prefix, AccessKey: r.cfg.S3.AccessKey, SecretKey: r.cfg.S3.SecretKey})
		var he *httpError
		if errors.As(err, &he) && he.Status >= 400 && he.Status < 500 && he.Status != http.StatusTooManyRequests {
			return true, fmt.Errorf("the console refused the target (HTTP %d): %s", he.Status, he.message())
		}
		return err == nil, err
	})
	if err != nil {
		return "", err
	}
	if key == "" {
		return "", errors.New("the console answered no recovery key")
	}
	r.mask(key)
	r.recoveryKey = key
	var s *backupSettings
	err = r.waitFor(ctx, 10*time.Minute, func(ctx context.Context) (bool, error) {
		var err error
		if s, err = r.console.backupSettings(ctx); err != nil {
			return false, err
		}
		if s.State != "Ready" {
			return false, fmt.Errorf("the backup target is %s: %s", s.State, s.Message)
		}
		return true, nil
	})
	if err != nil {
		return "", err
	}
	plan := "plan cluster created"
	if !planCreated {
		plan = "plan cluster existed"
	}
	return fmt.Sprintf("%s/%s, prefix %s, Ready; recovery key kept for the restore (masked); %s", r.cfg.S3.Endpoint, r.cfg.S3.Bucket, prefix, plan), nil
}

// seedRestoreData: a Volume with a marker file a Task wrote, served by an
// App; a SecretSet value whose hash another App answers.
func (r *runner) seedRestoreData(ctx context.Context, marker, secret string) (string, error) {
	files, secretHost := "files."+r.domain(), "secret."+r.domain()
	if err := r.createProject(ctx, restoreProject); err != nil {
		return "", fmt.Errorf("project: %w", err)
	}
	if err := r.retryCreate(ctx, func(ctx context.Context) error { return r.console.createVolume(ctx, restoreProject, "data", "1Gi") }); err != nil {
		return "", fmt.Errorf("volume: %w", err)
	}
	busybox := kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: busyboxImage}}
	data := []kwerftv1.AppVolume{{Path: "/www", Volume: "data"}}
	if err := r.createApp(ctx, restoreProject, "files", kwerftv1.AppSpec{
		Source: busybox, Command: []string{"httpd", "-f", "-p", "8080", "-h", "/www"},
		Ports: []kwerftv1.AppPort{{Container: 8080, Public: files}}, Volumes: data,
	}); err != nil {
		return "", fmt.Errorf("app files: %w", err)
	}
	task, err := r.console.createTask(ctx, restoreProject, kwerftv1.TaskSpec{
		Source:  &busybox,
		Command: []string{"sh", "-c", "echo " + marker + " >/www/marker.txt && sync"},
		Volumes: data,
		Timeout: &metav1.Duration{Duration: 5 * time.Minute},
	})
	if err != nil {
		return "", fmt.Errorf("task: %w", err)
	}
	if err := r.waitTask(ctx, restoreProject, task, 10*time.Minute); err != nil {
		return "", err
	}
	if err := r.waitHTTPS(ctx, "https://"+files+"/marker.txt", marker, 10*time.Minute); err != nil {
		return "", fmt.Errorf("the marker file: %w", err)
	}
	if err := r.retryCreate(ctx, func(ctx context.Context) error { return r.console.createSecretSet(ctx, restoreProject, "e2e") }); err != nil {
		return "", fmt.Errorf("secret set: %w", err)
	}
	if err := r.waitFor(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		err := r.console.setSecret(ctx, restoreProject, "e2e", "E2E_SECRET", secret)
		return err == nil, err
	}); err != nil {
		return "", fmt.Errorf("secret value: %w", err)
	}
	if err := r.createApp(ctx, restoreProject, "secret", kwerftv1.AppSpec{
		Source: busybox,
		Env: []corev1.EnvVar{{Name: "E2E_SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "e2e"}, Key: "E2E_SECRET"}}}},
		Command: []string{"sh", "-c", `mkdir -p /tmp/www && printf %s "$E2E_SECRET" | sha256sum | cut -d" " -f1 >/tmp/www/index.html && exec httpd -f -p 8080 -h /tmp/www`},
		Ports:   []kwerftv1.AppPort{{Container: 8080, Public: secretHost}},
	}); err != nil {
		return "", fmt.Errorf("app secret: %w", err)
	}
	if err := r.waitHTTPS(ctx, "https://"+secretHost+"/", sha256Hex(secret), 10*time.Minute); err != nil {
		return "", fmt.Errorf("the secret's hash: %w", err)
	}
	return fmt.Sprintf("project %s: volume data with marker.txt (Task %s) on https://%s; secret set e2e, https://%s answers the value's SHA-256", restoreProject, task, files, secretHost), nil
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// retryCreate retries a create while the project is being set up; a
// conflict means an earlier attempt made it.
func (r *runner) retryCreate(ctx context.Context, create func(context.Context) error) error {
	return r.waitFor(ctx, 2*time.Minute, func(ctx context.Context) (bool, error) {
		err := create(ctx)
		var he *httpError
		switch {
		case err == nil, errors.As(err, &he) && he.Status == http.StatusConflict:
			return true, nil
		case errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusUnprocessableEntity):
			return true, fmt.Errorf("HTTP %d: %s", he.Status, he.message())
		}
		return false, err
	})
}

// waitTask waits for a Task to succeed.
func (r *runner) waitTask(ctx context.Context, project, name string, timeout time.Duration) error {
	return r.waitFor(ctx, timeout, func(ctx context.Context) (bool, error) {
		t, err := r.console.task(ctx, project, name)
		if err != nil {
			return false, err
		}
		switch t.Status.Phase {
		case kwerftv1.TaskSucceeded:
			return true, nil
		case kwerftv1.TaskFailed:
			code := "unknown"
			if t.Status.ExitCode != nil {
				code = strconv.Itoa(int(*t.Status.ExitCode))
			}
			return true, fmt.Errorf("task %s failed (exit %s)", name, code)
		}
		return false, fmt.Errorf("task %s is %s", name, cmpOr(string(t.Status.Phase), "pending"))
	})
}

// backupNow runs the cluster plan and waits for its Backup.
func (r *runner) backupNow(ctx context.Context) (string, error) {
	var name string
	if err := r.waitFor(ctx, 3*time.Minute, func(ctx context.Context) (bool, error) {
		var err error
		name, err = r.console.runBackupPlan(ctx, "cluster")
		return err == nil && name != "", err
	}); err != nil {
		return "", fmt.Errorf("back up now: %w", err)
	}
	var b backupView
	err := r.waitFor(ctx, 45*time.Minute, func(ctx context.Context) (bool, error) {
		list, err := r.console.backups(ctx)
		if err != nil {
			return false, err
		}
		i := slices.IndexFunc(list, func(x backupView) bool { return x.Name == name })
		if i < 0 {
			return false, fmt.Errorf("backup %s is not listed yet", name)
		}
		b = list[i]
		switch b.Phase {
		case "Completed":
			return true, nil
		case "Failed", "PartiallyFailed", "FailedValidation":
			return true, fmt.Errorf("backup %s %s: %d error(s), %d warning(s) %s", name, b.Phase, b.Errors, b.Warnings, b.Message)
		}
		return false, fmt.Errorf("backup %s is %s", name, cmpOr(b.Phase, "New"))
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s Completed: %d items, %s of volume data", name, b.Items, fmtBytes(b.Bytes)), nil
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

const restoreDir = "/root/kwerft-e2e"

// uploadRestoreConfig writes the --config file with the backups block and
// the key files it names (0600, through stdin: never in a command line).
func (r *runner) uploadRestoreConfig(ctx context.Context) (string, error) {
	s := r.cfg.S3
	var y strings.Builder
	y.WriteString("# Written by the Kwerft e2e harness for install.sh --restore.\nbackups:\n")
	fmt.Fprintf(&y, "  endpoint: %s\n", s.Endpoint)
	if s.Region != "" {
		fmt.Fprintf(&y, "  region: %s\n", s.Region)
	}
	fmt.Fprintf(&y, "  bucket: %s\n  prefix: %s\n", s.Bucket, r.cfg.RunID)
	fmt.Fprintf(&y, "  accessKeyFile: %s/s3.access\n  secretKeyFile: %s/s3.secret\n  recoveryKeyFile: %s/recovery.key\n", restoreDir, restoreDir, restoreDir)
	for _, f := range []struct{ name, data string }{
		{"s3.access", s.AccessKey + "\n"},
		{"s3.secret", s.SecretKey + "\n"},
		{"recovery.key", r.recoveryKey + "\n"},
		{"kwerft.yaml", y.String()},
	} {
		if err := r.cur.remote.upload(ctx, restoreDir+"/"+f.name, []byte(f.data)); err != nil {
			return "", err
		}
	}
	return restoreDir + "/kwerft.yaml", nil
}

// appsRunning waits until the console says the Apps run.
func (r *runner) appsRunning(ctx context.Context, project string, apps ...string) (string, error) {
	for _, name := range apps {
		err := r.waitFor(ctx, 10*time.Minute, func(ctx context.Context) (bool, error) {
			a, err := r.console.app(ctx, project, name)
			if err != nil {
				return false, err
			}
			if a.Phase != "running" {
				return false, fmt.Errorf("app %s is %s: %s %s", name, a.Phase, a.Reason, a.Message)
			}
			return true, nil
		})
		if err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("%s: %s running", project, strings.Join(apps, ", ")), nil
}

// secretRestored: the key is in the set, and the App that reads it answers
// the hash of the value written before the backup.
func (r *runner) secretRestored(ctx context.Context, secret string) (string, error) {
	keys, err := r.console.secretKeys(ctx, restoreProject, "e2e")
	if err != nil {
		return "", err
	}
	if !slices.Contains(keys, "E2E_SECRET") {
		return "", fmt.Errorf("secret set e2e has the keys %v, not E2E_SECRET", keys)
	}
	u := "https://secret." + r.domain() + "/"
	if err := r.waitHTTPS(ctx, u, sha256Hex(secret), 10*time.Minute); err != nil {
		return "", fmt.Errorf("the App reading the value: %w", err)
	}
	return "secret set e2e has E2E_SECRET; " + u + " answers the SHA-256 of the value from before the backup", nil
}

// dnsReport says where the restored console's names point.
func (r *runner) dnsReport(old *machine) string {
	s := fmt.Sprintf("the console and its Apps kept %s from the backup; as an sslip.io name it points at the deleted server %s, so these checks connected to the new server %s with the old names (as DNS would once moved)",
		r.domain(), old.ip, r.cur.ip)
	if len(r.restoreDNS) > 0 {
		s += "; the installer said: " + strings.Join(r.restoreDNS, " / ")
	}
	r.rep.note("Restore: a console on its own domain needs its DNS records moved to the new server (%s), or Kwerft moves them itself with dns.manageRecords.", r.cur.ip)
	return s
}
