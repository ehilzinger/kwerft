// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// The runner (`kwerft upgrade-runner`) drives a Kwerft Upgrade from the
// backup to verification and rolls it back: it runs the *current* console
// image, so a broken new release cannot take its own rollback down. Every
// step is idempotent and the Upgrade's phase says where to resume, so a
// restarted runner pod (the Job retries) picks up where the last one was.
//
//	Backup ─► download + checksum, disk, App baseline, etcd snapshot, Helm revisions
//	Running ─► systemd-run install.sh --progress; tail progress into status.steps
//	Verifying ─► Verify against the target (VerifyTimeout)
//	RollingBack ─► helm rollback (reverse stage order), Verify against the old version
//	Succeeded | RolledBack | Failed | Cancelled ─► log tail ConfigMap, unit removed

// Command is a program the runner starts on the host.
type Command struct {
	// Unit names the transient systemd unit; Run makes it unique.
	Unit string
	Env  []string
	Args []string
}

// UnitState is a transient unit's state on the host.
type UnitState struct {
	Exists   bool
	Running  bool
	Finished bool
	ExitCode int
}

// Host is what the runner does on the installer node, as root: every
// program runs as a transient systemd unit, so it outlives the runner pod.
type Host interface {
	// Run runs a command to completion and returns its output.
	Run(ctx context.Context, cmd Command) (string, error)
	// Start starts a command in the background, as unit cmd.Unit, which
	// keeps its exit status after it ended (RemainAfterExit).
	Start(ctx context.Context, cmd Command) error
	Unit(ctx context.Context, name string) (UnitState, error)
	// Remove stops a unit and forgets it.
	Remove(ctx context.Context, name string) error
	// FreeBytes is the free space of a host path.
	FreeBytes(path string) (uint64, error)
	// Exists reports whether a host path exists.
	Exists(path string) bool
	// LogTail is the end of the installer log.
	LogTail(n int) ([]byte, error)
}

// Fetcher downloads files of the install repository (HTTPSource).
type Fetcher interface {
	Fetch(ctx context.Context, path string) ([]byte, error)
}

// Fetch implements Fetcher.
func (s *HTTPSource) Fetch(ctx context.Context, path string) ([]byte, error) { return s.get(ctx, path) }

// VerifyTarget is what a verification expects.
type VerifyTarget struct {
	// Version the console must report.
	Version string
	// Apps: ready replicas per "<namespace>/<app>" before the upgrade; no
	// App may have fewer.
	Apps map[string]int32
}

// Cluster is the runner's side of Kubernetes.
type Cluster interface {
	Upgrade(ctx context.Context) (*kwerftv1.Upgrade, error)
	// UpdateStatus applies mutate to the newest Upgrade and writes its
	// status, retrying conflicts; an error from mutate is returned as is.
	UpdateStatus(ctx context.Context, mutate func(*kwerftv1.Upgrade) error) (*kwerftv1.Upgrade, error)
	Annotate(ctx context.Context, key, value string) error
	AppReplicas(ctx context.Context) (map[string]int32, error)
	// Verify lists what is not (yet) as expected; empty means verified.
	Verify(ctx context.Context, t VerifyTarget) []string
	WriteLog(ctx context.Context, data []byte) error
}

// Runner is one Upgrade's runner.
type Runner struct {
	Name    string
	Cluster Cluster
	Host    Host
	Files   Fetcher
	// Dir is this Upgrade's directory as the runner sees it; HostDir is the
	// same directory on the host (HostWorkDir/<name>).
	Dir, HostDir string
	// Fault injects a failure (AnnotationFault; e2e only).
	Fault string

	Poll           time.Duration // default 5s
	VerifyTimeout  time.Duration // default 10m
	InstallTimeout time.Duration // default 90m
	Now            func() time.Time
	Log            *slog.Logger
}

const (
	defaultPoll           = 5 * time.Second
	defaultVerifyTimeout  = 10 * time.Minute
	defaultInstallTimeout = 90 * time.Minute

	fileInstaller = "install.sh"
	fileSums      = "SHA256SUMS"
	fileManifest  = "manifest.json"
	fileProgress  = "progress.jsonl"
	fileApps      = "apps-before.json"
	fileStarted   = "started"
	// fileSnapshotRecorded: a Kubernetes upgrade's etcd snapshot is in its
	// status (written after the status update).
	fileSnapshotRecorded = "snapshot-recorded"
)

var errPhaseMoved = errors.New("the upgrade moved on")

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (r *Runner) sleep(ctx context.Context) error {
	t := time.NewTimer(orDefault(r.Poll, defaultPoll))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run drives the Upgrade until it is finished. An error is transient (the
// Kubernetes API, the host's systemd): the pod exits non-zero and the Job's
// next pod resumes.
func (r *Runner) Run(ctx context.Context) error {
	for {
		u, err := r.Cluster.Upgrade(ctx)
		if err != nil {
			return err
		}
		switch u.Spec.Component {
		case kwerftv1.UpgradeKubernetes:
			return r.snapshot(ctx, u)
		case kwerftv1.UpgradeKwerft:
		default:
			return fmt.Errorf("upgrade %s is a %s upgrade; the runner upgrades Kwerft and backs up Kubernetes", u.Name, u.Spec.Component)
		}
		phase := u.Status.Phase
		r.log().Info("upgrade runner", "upgrade", u.Name, "phase", phase)
		switch {
		case phase == kwerftv1.UpgradeBackingUp:
			err = r.prepare(ctx, u)
		case phase == kwerftv1.UpgradeRunning:
			err = r.install(ctx, u)
		case phase == kwerftv1.UpgradeVerifying:
			err = r.verify(ctx, u)
		case phase == kwerftv1.UpgradeRollingBack:
			err = r.rollback(ctx, u)
		case Finished(phase):
			return r.finish(ctx, u)
		default:
			return fmt.Errorf("upgrade %s is %q: nothing for the runner yet", u.Name, phase)
		}
		if err != nil {
			return err
		}
	}
}

// move changes the phase from `from`, unless something else moved it first.
func (r *Runner) move(ctx context.Context, from kwerftv1.UpgradePhase, mutate func(*kwerftv1.Upgrade)) error {
	_, err := r.Cluster.UpdateStatus(ctx, func(u *kwerftv1.Upgrade) error {
		if u.Status.Phase != from {
			return errPhaseMoved
		}
		mutate(u)
		return nil
	})
	if errors.Is(err, errPhaseMoved) {
		return nil
	}
	return err
}

func (r *Runner) finished(u *kwerftv1.Upgrade, phase kwerftv1.UpgradePhase) {
	u.Status.Phase = phase
	u.Status.FinishedAt = &metav1.Time{Time: r.now()}
}

// failBefore ends an Upgrade that changed nothing yet.
func (r *Runner) failBefore(ctx context.Context, reason, msg string) error {
	return r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) {
		r.finished(u, kwerftv1.UpgradeFailed)
		u.Status.Reason, u.Status.Message = reason, sentence(msg)+" Nothing was changed."
	})
}

// sentence ends s with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, "?") {
		return s
	}
	return s + "."
}

// prepare is the Backup phase.
func (r *Runner) prepare(ctx context.Context, u *kwerftv1.Upgrade) error {
	if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		return r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) { cancelled(u, by, r.now()) })
	}
	if free, err := r.Host.FreeBytes("/var/lib"); err != nil {
		return fmt.Errorf("free disk space: %w", err)
	} else if free < MinFreeBytes {
		return r.failBefore(ctx, "Preflight", fmt.Sprintf("Only %s free in /var/lib of the installer node; %s needed.", gib(free), gib(MinFreeBytes)))
	}
	// The installer takes every flag but --version from install.env, which
	// releases before Phase 6 never wrote.
	if !r.Host.Exists(HostInstallEnv) {
		return r.failBefore(ctx, "Preflight", "The installer node has no "+HostInstallEnv+
			": it was installed by a release from before console upgrades. Re-run the installer of the running release there once.")
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	if msg, err := r.download(ctx, u.Spec.Version); err != nil {
		return err
	} else if msg != "" {
		return r.failBefore(ctx, "Preflight", msg)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, fileApps)); errors.Is(err, os.ErrNotExist) {
		apps, err := r.Cluster.AppReplicas(ctx)
		if err != nil {
			return fmt.Errorf("App replicas: %w", err)
		}
		raw, _ := json.Marshal(apps)
		if err := writeFile(filepath.Join(r.Dir, fileApps), raw, 0o600); err != nil {
			return err
		}
	}
	if u.Status.Backup == nil || u.Status.Backup.EtcdSnapshot == "" {
		name := SnapshotName(u.Name)
		if out, err := r.Host.Run(ctx, Command{Unit: UnitName(u.Name) + "-snapshot",
			Args: []string{HostK3s, "etcd-snapshot", "save", "--name", name}}); err != nil {
			return r.failBefore(ctx, "Backup", "The etcd snapshot failed: "+lastLine(out, err))
		}
		if err := r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) {
			if u.Status.Backup == nil {
				u.Status.Backup = &kwerftv1.UpgradeBackup{}
			}
			u.Status.Backup.EtcdSnapshot = name
		}); err != nil {
			return err
		}
	}
	revisions, err := r.helmRevisions(ctx)
	if err != nil {
		return r.failBefore(ctx, "Backup", "Cannot read the Helm releases: "+err.Error())
	}
	return r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) {
		if by := u.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
			cancelled(u, by, r.now())
			return
		}
		if u.Status.Backup == nil {
			u.Status.Backup = &kwerftv1.UpgradeBackup{}
		}
		u.Status.Backup.HelmRevisions = revisions
		u.Status.Phase = kwerftv1.UpgradeRunning
		u.Status.Message = "Running the installer of " + u.Spec.Version + "."
	})
}

// snapshot is all the runner does for a Kubernetes upgrade: the etcd
// snapshot of its Backup phase, on the installer node. The Upgrade
// controller (internal/controllers/k3s_upgrade.go) moves the phase on and
// drives the nodes through system-upgrade-controller; a cancel in Backup is
// its too.
func (r *Runner) snapshot(ctx context.Context, u *kwerftv1.Upgrade) error {
	if u.Status.Phase != kwerftv1.UpgradeBackingUp || (u.Status.Backup != nil && u.Status.Backup.EtcdSnapshot != "") ||
		u.Annotations[kwerftv1.AnnotationCancelRequested] != "" {
		r.log().Info("nothing to back up", "upgrade", u.Name, "phase", u.Status.Phase)
		return nil
	}
	name := SnapshotName(u.Name)
	// The snapshot is taken while the Upgrade is in Backup, so a cluster
	// restored from it has the Upgrade in Backup again, without the
	// snapshot recorded. The marker on the host (which the restore does not
	// reset) tells that from a retried runner, and the upgrade is not
	// repeated.
	marker := filepath.Join(r.Dir, fileSnapshotRecorded)
	if _, err := os.Stat(marker); err == nil {
		return r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) {
			r.finished(u, kwerftv1.UpgradeFailed)
			u.Status.Reason = "Backup"
			u.Status.Message = "This cluster was restored from this upgrade's etcd snapshot " + name +
				": the upgrade is not repeated. Start a new one when the cause is fixed."
		})
	}
	if free, err := r.Host.FreeBytes("/var/lib"); err != nil {
		return fmt.Errorf("free disk space: %w", err)
	} else if free < MinFreeBytesKubernetes {
		return r.failBefore(ctx, "Preflight", fmt.Sprintf("Only %s free in /var/lib of the installer node; %s needed for the etcd snapshot.",
			gib(free), gib(MinFreeBytesKubernetes)))
	}
	if out, err := r.Host.Run(ctx, Command{Unit: UnitName(u.Name) + "-snapshot",
		Args: []string{HostK3s, "etcd-snapshot", "save", "--name", name}}); err != nil {
		return r.failBefore(ctx, "Backup", "The etcd snapshot failed: "+lastLine(out, err))
	}
	r.log().Info("etcd snapshot taken", "upgrade", u.Name, "snapshot", name)
	if err := r.move(ctx, kwerftv1.UpgradeBackingUp, func(u *kwerftv1.Upgrade) {
		if u.Status.Backup == nil {
			u.Status.Backup = &kwerftv1.UpgradeBackup{}
		}
		u.Status.Backup.EtcdSnapshot = name
	}); err != nil {
		return err
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	return writeFile(marker, []byte(name+"\n"), 0o600)
}

func cancelled(u *kwerftv1.Upgrade, by string, now time.Time) {
	u.Status.Phase = kwerftv1.UpgradeCancelled
	u.Status.FinishedAt = &metav1.Time{Time: now}
	u.Status.Reason = "Cancelled"
	u.Status.Message = "Cancelled by " + by + " before anything changed."
}

// download fetches the release's installer, checksums and manifest and
// verifies them. A non-empty message is a failure of the release itself.
func (r *Runner) download(ctx context.Context, version string) (string, error) {
	prefix := "v" + strings.TrimPrefix(version, "v") + "/"
	files := map[string][]byte{}
	for _, f := range []string{fileSums, fileInstaller, fileManifest} {
		body, err := r.Files.Fetch(ctx, prefix+f)
		if errors.Is(err, ErrNotFound) {
			return fmt.Sprintf("Release %s has no %s in the install repository.", version, f), nil
		}
		if err != nil {
			return "", fmt.Errorf("download %s: %w", f, err)
		}
		files[f] = body
	}
	want, ok := ParseSums(files[fileSums])[fileInstaller]
	if !ok {
		return fmt.Sprintf("SHA256SUMS of %s does not list install.sh.", version), nil
	}
	sum := sha256.Sum256(files[fileInstaller])
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Sprintf("install.sh of %s does not match its SHA256SUMS (got %s, want %s).", version, got, want), nil
	}
	var m Manifest
	if err := json.Unmarshal(files[fileManifest], &m); err != nil {
		return fmt.Sprintf("manifest.json of %s is not valid: %v.", version, err), nil
	}
	if m.Version != strings.TrimPrefix(version, "v") {
		return fmt.Sprintf("manifest.json of %s names version %q.", version, m.Version), nil
	}
	for f, body := range files {
		mode := os.FileMode(0o600)
		if f == fileInstaller {
			mode = 0o700
		}
		if err := writeFile(filepath.Join(r.Dir, f), body, mode); err != nil {
			return "", err
		}
	}
	return "", nil
}

// ParseSums reads sha256sum output: "<hex>  <file>" per line.
func ParseSums(data []byte) map[string]string {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		out[strings.TrimPrefix(fields[1], "*")] = strings.ToLower(fields[0])
	}
	return out
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func gib(b uint64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

func lastLine(out string, err error) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return truncate(l, 300)
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

// helmRelease is one entry of `helm list -A -o json` (revision is a string
// in Helm 3, a number elsewhere).
type helmRelease struct {
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Revision  json.RawMessage `json:"revision"`
}

// ParseHelmList reads `helm list -A -o json` into "<namespace>/<name>" →
// revision.
func ParseHelmList(out string) (map[string]int32, error) {
	start := strings.IndexByte(out, '[')
	if start < 0 {
		return nil, fmt.Errorf("helm list: no JSON in %q", truncate(out, 200))
	}
	var list []helmRelease
	if err := json.Unmarshal([]byte(out[start:]), &list); err != nil {
		return nil, fmt.Errorf("helm list: %w", err)
	}
	revs := map[string]int32{}
	for _, rel := range list {
		raw := strings.Trim(string(rel.Revision), `"`)
		n, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("helm list: revision %q of %s", raw, rel.Name)
		}
		revs[rel.Namespace+"/"+rel.Name] = int32(n)
	}
	return revs, nil
}

func (r *Runner) helmEnv() []string {
	return []string{"KUBECONFIG=" + HostKubeconfig, "HOME=/root"}
}

// HelmListArgs lists every release's current revision, failed and pending
// ones included. Helm 4 (what install.sh installs) has no --all; these state
// flags mean the same in Helm 3 and 4.
var HelmListArgs = []string{"list", "--all-namespaces", "--deployed", "--failed", "--pending", "--output", "json"}

func (r *Runner) helmRevisions(ctx context.Context) (map[string]int32, error) {
	out, err := r.Host.Run(ctx, Command{Unit: UnitName(r.Name) + "-helm", Env: r.helmEnv(),
		Args: append([]string{HostHelm}, HelmListArgs...)})
	if err != nil {
		return nil, errors.New(lastLine(out, err))
	}
	return ParseHelmList(out)
}

func (r *Runner) readProgress() Progress {
	data, err := os.ReadFile(filepath.Join(r.Dir, fileProgress))
	if err != nil {
		return Progress{}
	}
	return ParseProgress(data)
}

// install is the Running phase: start the installer once, then follow it.
func (r *Runner) install(ctx context.Context, u *kwerftv1.Upgrade) error {
	unit := UnitName(u.Name)
	st, err := r.Host.Unit(ctx, unit)
	if err != nil {
		return err
	}
	marker := filepath.Join(r.Dir, fileStarted)
	_, markerErr := os.Stat(marker)
	if !st.Exists && markerErr != nil && r.readProgress().Exit == nil {
		if err := writeFile(marker, []byte(r.now().UTC().Format(time.RFC3339)), 0o600); err != nil {
			return err
		}
		if err := r.Host.Start(ctx, Command{Unit: unit, Env: []string{"HOME=/root"}, Args: []string{
			filepath.Join(r.HostDir, fileInstaller), "--version", u.Spec.Version, "--yes",
			"--progress", filepath.Join(r.HostDir, fileProgress),
		}}); err != nil {
			_ = os.Remove(marker)
			return fmt.Errorf("start the installer: %w", err)
		}
		r.log().Info("installer started", "unit", unit, "version", u.Spec.Version)
	}
	started := r.now()
	if data, err := os.ReadFile(marker); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data))); err == nil {
			started = t
		}
	}
	steps := u.Status.Steps
	for {
		prog := r.readProgress()
		if prog.Steps != nil && !equality.Semantic.DeepEqual(prog.Steps, steps) {
			if err := r.move(ctx, kwerftv1.UpgradeRunning, func(u *kwerftv1.Upgrade) { u.Status.Steps = prog.Steps }); err != nil {
				return err
			}
			steps = prog.Steps
			r.writeLog(ctx) // the log so far, once per stage
		}
		st, err := r.Host.Unit(ctx, unit)
		if err != nil {
			return err
		}
		code, done := 0, false
		switch {
		case st.Finished:
			code, done = st.ExitCode, true
			if prog.Exit != nil {
				code = *prog.Exit
			}
		case !st.Exists && prog.Exit != nil:
			code, done = *prog.Exit, true
		case !st.Exists:
			return r.installFailed(ctx, "Installer", "The installer's unit "+unit+" ended without an exit code (did the server restart?).")
		}
		if done {
			prog = r.readProgress()
			if prog.Steps != nil {
				steps = prog.Steps
			}
			if code == 0 && r.Fault == FaultInstall {
				code = 50
				r.log().Warn("fault injected: the installer counts as failed", "upgrade", u.Name)
			}
			if code == 0 {
				return r.move(ctx, kwerftv1.UpgradeRunning, func(u *kwerftv1.Upgrade) {
					u.Status.Steps = steps
					u.Status.Phase = kwerftv1.UpgradeVerifying
					u.Status.Message = "Verifying " + u.Spec.Version + "."
				})
			}
			msg := fmt.Sprintf("The installer exited with code %d.", code)
			if f := failedStep(steps); f != nil {
				msg = fmt.Sprintf("The installer failed in stage %s (exit %d): %s", f.Label, code, f.Detail)
			}
			if r.Fault == FaultInstall {
				msg = "Fault injected (e2e): the installer counts as failed."
			}
			return r.installFailedSteps(ctx, ExitReason(code), msg, steps)
		}
		if r.now().Sub(started) > orDefault(r.InstallTimeout, defaultInstallTimeout) {
			if err := r.Host.Remove(ctx, unit); err != nil {
				return err
			}
			return r.installFailed(ctx, "Timeout", fmt.Sprintf("The installer ran longer than %s and was stopped.", orDefault(r.InstallTimeout, defaultInstallTimeout)))
		}
		if err := r.sleep(ctx); err != nil {
			return err
		}
	}
}

func failedStep(steps []kwerftv1.UpgradeStep) *kwerftv1.UpgradeStep {
	for i := range steps {
		if steps[i].State == StepFailed {
			return &steps[i]
		}
	}
	return nil
}

func (r *Runner) installFailed(ctx context.Context, reason, msg string) error {
	return r.move(ctx, kwerftv1.UpgradeRunning, func(u *kwerftv1.Upgrade) {
		u.Status.Phase = kwerftv1.UpgradeRollingBack
		u.Status.Reason, u.Status.Message = reason, msg
	})
}

func (r *Runner) installFailedSteps(ctx context.Context, reason, msg string, steps []kwerftv1.UpgradeStep) error {
	return r.move(ctx, kwerftv1.UpgradeRunning, func(u *kwerftv1.Upgrade) {
		u.Status.Steps = steps
		u.Status.Phase = kwerftv1.UpgradeRollingBack
		u.Status.Reason, u.Status.Message = reason, msg
	})
}

func (r *Runner) baseline() map[string]int32 {
	apps := map[string]int32{}
	if data, err := os.ReadFile(filepath.Join(r.Dir, fileApps)); err == nil {
		_ = json.Unmarshal(data, &apps)
	}
	return apps
}

// waitVerified verifies until it passes or the timeout ends; it returns
// the problems left.
func (r *Runner) waitVerified(ctx context.Context, t VerifyTarget, progress func([]string) error) ([]string, error) {
	deadline := r.now().Add(orDefault(r.VerifyTimeout, defaultVerifyTimeout))
	var last []string
	for {
		problems := r.Cluster.Verify(ctx, t)
		if len(problems) == 0 {
			return nil, nil
		}
		if progress != nil && !slices.Equal(problems, last) {
			if err := progress(problems); err != nil {
				return nil, err
			}
		}
		last = problems
		if !r.now().Before(deadline) {
			return problems, nil
		}
		if err := r.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

// verify is the Verifying phase.
func (r *Runner) verify(ctx context.Context, u *kwerftv1.Upgrade) error {
	if r.Fault == FaultVerify {
		r.log().Warn("fault injected: verification fails", "upgrade", u.Name)
		return r.move(ctx, kwerftv1.UpgradeVerifying, func(u *kwerftv1.Upgrade) {
			u.Status.Phase = kwerftv1.UpgradeRollingBack
			u.Status.Reason, u.Status.Message = "Verify", "Fault injected (e2e): verification failed."
		})
	}
	problems, err := r.waitVerified(ctx, VerifyTarget{Version: u.Spec.Version, Apps: r.baseline()}, func(p []string) error {
		return r.move(ctx, kwerftv1.UpgradeVerifying, func(u *kwerftv1.Upgrade) {
			u.Status.Message = "Verifying: " + strings.Join(p, "; ")
		})
	})
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return r.move(ctx, kwerftv1.UpgradeVerifying, func(u *kwerftv1.Upgrade) {
			u.Status.Phase = kwerftv1.UpgradeRollingBack
			u.Status.Reason = "Verify"
			u.Status.Message = "Verification failed: " + strings.Join(problems, "; ")
		})
	}
	return r.move(ctx, kwerftv1.UpgradeVerifying, func(u *kwerftv1.Upgrade) {
		r.finished(u, kwerftv1.UpgradeSucceeded)
		u.Status.Reason = ""
		u.Status.Message = "Kwerft " + u.Spec.Version + " is running."
	})
}

// releaseOrder is the installer's stage order of its Helm releases; a
// rollback goes the other way (Kwerft first, Cilium last). Releases not
// listed (added by later releases) are rolled back right after Kwerft.
var releaseOrder = []string{
	"kube-system/cilium",
	"kube-system/hcloud-cloud-controller-manager",
	"kube-system/hcloud-csi",
	"cert-manager/cert-manager",
	"cert-manager/cert-manager-webhook-hetzner",
	"traefik/traefik",
	"kwerft-observability/vm",
	"kwerft-observability/vlogs",
	"kwerft-system/kwerft",
}

// RollbackStep is one `helm rollback`.
type RollbackStep struct {
	Release  string // namespace/name
	Revision int32
}

// RollbackPlan lists the releases whose revision changed since before, in
// rollback order, and the releases that are new (left installed).
func RollbackPlan(before, now map[string]int32) (steps []RollbackStep, added []string) {
	for rel, rev := range before {
		if cur, ok := now[rel]; ok && cur != rev {
			steps = append(steps, RollbackStep{Release: rel, Revision: rev})
		}
	}
	for rel := range now {
		if _, ok := before[rel]; !ok {
			added = append(added, rel)
		}
	}
	rank := func(rel string) int {
		if i := slices.Index(releaseOrder, rel); i >= 0 {
			return 2 * i
		}
		return 2*(len(releaseOrder)-1) - 1 // between the last platform release and Kwerft
	}
	slices.SortFunc(steps, func(a, b RollbackStep) int {
		if d := rank(b.Release) - rank(a.Release); d != 0 {
			return d
		}
		return strings.Compare(a.Release, b.Release)
	})
	slices.Sort(added)
	return steps, added
}

func (r *Runner) manifest() *Manifest {
	data, err := os.ReadFile(filepath.Join(r.Dir, fileManifest))
	if err != nil {
		return nil
	}
	var m Manifest
	if json.Unmarshal(data, &m) != nil {
		return nil
	}
	return &m
}

// rollback is the RollingBack phase.
func (r *Runner) rollback(ctx context.Context, u *kwerftv1.Upgrade) error {
	unit := UnitName(u.Name)
	if st, err := r.Host.Unit(ctx, unit); err != nil {
		return err
	} else if st.Running {
		if err := r.Host.Remove(ctx, unit); err != nil {
			return err
		}
	}
	var before map[string]int32
	if u.Status.Backup != nil {
		before = u.Status.Backup.HelmRevisions
	}
	now, err := r.helmRevisions(ctx)
	if err != nil {
		return err
	}
	plan, added := RollbackPlan(before, now)
	from := ""
	if u.Status.From != nil {
		from = u.Status.From.Kwerft
	}
	m := r.manifest()
	restoreDB := m != nil && !m.IsRollbackSafe() && u.Spec.AcceptDataRollback && from != "" &&
		slices.ContainsFunc(plan, func(s RollbackStep) bool { return s.Release == "kwerft-system/kwerft" })
	if restoreDB && u.Annotations[AnnotationRestoreDatabase] == "" {
		if err := r.Cluster.Annotate(ctx, AnnotationRestoreDatabase, from); err != nil {
			return err
		}
	}
	var failures, manual []string
	for _, s := range plan {
		ns, name, _ := strings.Cut(s.Release, "/")
		rev := strconv.Itoa(int(s.Revision))
		manual = append(manual, fmt.Sprintf("helm rollback %s %s -n %s --wait", name, rev, ns))
		out, err := r.Host.Run(ctx, Command{Unit: unit + "-rollback", Env: r.helmEnv(),
			Args: []string{HostHelm, "rollback", name, rev, "--namespace", ns, "--wait", "--timeout", "10m"}})
		if err != nil {
			failures = append(failures, fmt.Sprintf("helm rollback %s: %s", s.Release, lastLine(out, err)))
		}
		r.log().Info("helm rollback", "release", s.Release, "revision", s.Revision, "err", err)
	}
	var problems []string
	if from != "" {
		problems, err = r.waitVerified(ctx, VerifyTarget{Version: from, Apps: r.baseline()}, nil)
		if err != nil {
			return err
		}
	}
	cause := sentence(u.Status.Message)
	if len(failures) == 0 && len(problems) == 0 {
		return r.move(ctx, kwerftv1.UpgradeRollingBack, func(u *kwerftv1.Upgrade) {
			r.finished(u, kwerftv1.UpgradeRolledBack)
			msg := cause + " Rolled back"
			if from != "" {
				msg += " to " + from
			}
			if len(plan) == 0 {
				msg += " (no Helm release had changed)"
			}
			msg += "."
			if restoreDB {
				msg += " The database was restored from " + backupPath(u) + "."
			}
			if len(added) > 0 {
				msg += " Left installed (new in " + u.Spec.Version + "): " + strings.Join(added, ", ") + "."
			}
			u.Status.Message = msg
		})
	}
	return r.move(ctx, kwerftv1.UpgradeRollingBack, func(u *kwerftv1.Upgrade) {
		r.finished(u, kwerftv1.UpgradeFailed)
		msg := cause + " The rollback did not complete: " + strings.Join(append(failures, problems...), "; ") + "."
		if len(manual) > 0 {
			msg += " By hand, on the server the installer ran on: " + strings.Join(manual, "; ") + "."
		}
		if b := u.Status.Backup; b != nil && b.EtcdSnapshot != "" {
			msg += " The etcd snapshot " + b.EtcdSnapshot + " (k3s etcd-snapshot ls) is kept"
			if b.Database != "" {
				msg += ", and the database copy " + b.Database
			}
			msg += "."
		}
		u.Status.Message = msg
	})
}

func backupPath(u *kwerftv1.Upgrade) string {
	if u.Status.Backup != nil {
		return u.Status.Backup.Database
	}
	return ""
}

// writeLog copies the installer log's tail into the log ConfigMap, best
// effort (while the installer runs).
func (r *Runner) writeLog(ctx context.Context) {
	tail, err := r.Host.LogTail(LogTailBytes)
	if err == nil {
		err = r.Cluster.WriteLog(ctx, tail)
	}
	if err != nil {
		r.log().Warn("cannot copy the installer log", "err", err)
	}
}

// finish keeps the installer log's tail and removes the unit.
func (r *Runner) finish(ctx context.Context, u *kwerftv1.Upgrade) error {
	if tail, err := r.Host.LogTail(LogTailBytes); err == nil {
		if err := r.Cluster.WriteLog(ctx, tail); err != nil {
			return err
		}
	} else {
		r.log().Warn("cannot read the installer log", "err", err)
	}
	if err := r.Host.Remove(ctx, UnitName(u.Name)); err != nil {
		r.log().Warn("cannot remove the installer unit", "err", err)
	}
	r.log().Info("upgrade finished", "upgrade", u.Name, "phase", u.Status.Phase, "message", u.Status.Message)
	return nil
}
