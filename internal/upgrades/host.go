package upgrades

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// SystemdHost is the runner's Host: the host's root file system is mounted
// read-only at Root, and only systemd-run --wait --pipe runs there
// (chrooted, CAP_SYS_CHROOT), talking to the host's systemd over the system
// bus.
// Everything else — k3s, helm, install.sh, even systemctl — runs as a
// transient unit of the host's systemd, with the host's full environment,
// so it outlives the runner pod and sees the host as the installer does.
//
// systemctl does not run in the chroot itself: as root it talks to
// systemd's private socket and checks the peer's credentials, and pid 1 of
// the host is not in the pod's PID namespace ("No data available").
// systemd-run goes through the D-Bus daemon, which an AppArmor-confined
// caller cannot reach: the runner pod is Unconfined
// (internal/controllers/upgrade_runner.go).
type SystemdHost struct {
	// Root is where the host's / is mounted ("/" runs directly).
	Root string
	// Exec runs a program in the chroot (tests replace it).
	Exec func(ctx context.Context, root string, args ...string) (string, error)
}

const (
	systemdRun = "/usr/bin/systemd-run"
	systemctl  = "/usr/bin/systemctl"
)

func (h *SystemdHost) exec(ctx context.Context, args ...string) (string, error) {
	if h.Exec != nil {
		return h.Exec(ctx, h.Root, args...)
	}
	return chrootExec(ctx, h.Root, args...)
}

func chrootExec(ctx context.Context, root string, args ...string) (string, error) {
	cmd := execCommand(ctx, args[0], args[1:]...)
	if root != "" && root != "/" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: root}
	}
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C"}
	cmd.Stdin = strings.NewReader("")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

func unitArgs(cmd Command) []string {
	var args []string
	for _, e := range cmd.Env {
		args = append(args, "--setenv="+e)
	}
	return append(append(args, "--"), cmd.Args...)
}

// Run runs cmd as `systemd-run --wait --pipe` under a unique unit name.
func (h *SystemdHost) Run(ctx context.Context, cmd Command) (string, error) {
	unit := fmt.Sprintf("%s-%s", cmd.Unit, strconv.FormatInt(time.Now().UnixNano(), 36))
	args := append([]string{systemdRun, "--unit=" + unit, "--wait", "--pipe", "--quiet", "--collect",
		"--service-type=exec", "--description=Kwerft upgrade"}, unitArgs(cmd)...)
	return h.exec(ctx, args...)
}

// Start starts cmd as unit cmd.Unit. RemainAfterExit keeps its exit status
// readable; KillMode=process lets the k3s and helm processes it started
// outlive a stop.
//
// A systemd-run that neither waits nor pipes talks to systemd's private
// socket, which fails from the pod like systemctl does (see SystemdHost), so
// that systemd-run runs on the host, in a short unit Run waits for; it
// returns once the installer's unit is started.
func (h *SystemdHost) Start(ctx context.Context, cmd Command) error {
	args := append([]string{systemdRun, "--unit=" + cmd.Unit, "--quiet",
		"--property=RemainAfterExit=yes", "--property=KillMode=process", "--property=TimeoutStopSec=30",
		"--description=Kwerft upgrade (install.sh)"}, unitArgs(cmd)...)
	out, err := h.Run(ctx, Command{Unit: cmd.Unit + "-start", Args: args})
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return nil
}

// Unit reads `systemctl show`.
func (h *SystemdHost) Unit(ctx context.Context, name string) (UnitState, error) {
	out, err := h.systemctl(ctx, name, "show", name+".service",
		"--property=LoadState,ActiveState,SubState,ExecMainStatus,ExecMainExitTimestampMonotonic")
	if err != nil {
		return UnitState{}, fmt.Errorf("systemctl show %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	return ParseUnitState(out), nil
}

// ParseUnitState reads `systemctl show` key=value lines.
func ParseUnitState(out string) UnitState {
	props := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	var st UnitState
	if props["LoadState"] == "" || props["LoadState"] == "not-found" {
		return st
	}
	st.Exists = true
	st.ExitCode, _ = strconv.Atoi(props["ExecMainStatus"])
	exited := props["ExecMainExitTimestampMonotonic"] != "" && props["ExecMainExitTimestampMonotonic"] != "0"
	switch active, sub := props["ActiveState"], props["SubState"]; {
	case active == "failed":
		st.Finished = true
		if st.ExitCode == 0 {
			st.ExitCode = 1 // killed by a signal
		}
	case active == "active" && sub == "exited", active == "inactive" && exited:
		st.Finished = true
	case active == "inactive":
		st.Exists = false // loaded but never ran
	default:
		st.Running = true
	}
	return st
}

// Remove stops a unit and resets a failed one.
func (h *SystemdHost) Remove(ctx context.Context, name string) error {
	if out, err := h.systemctl(ctx, name, "stop", name+".service"); err != nil && !strings.Contains(out, "not loaded") {
		return fmt.Errorf("systemctl stop %s: %w: %s", name, err, strings.TrimSpace(out))
	}
	_, _ = h.systemctl(ctx, name, "reset-failed", name+".service")
	return nil
}

// systemctl runs systemctl on the host as a transient unit named after the
// unit it acts on (see SystemdHost).
func (h *SystemdHost) systemctl(ctx context.Context, unit string, args ...string) (string, error) {
	return h.Run(ctx, Command{Unit: unit + "-" + args[0], Args: append([]string{systemctl}, args...)})
}

func (h *SystemdHost) hostPath(p string) string { return filepath.Join(h.Root, p) }

func (h *SystemdHost) FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(h.hostPath(path), &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // the field types differ by OS
}

var execCommand = exec.CommandContext

func (h *SystemdHost) LogTail(n int) ([]byte, error) { return FileTail(h.hostPath(HostInstallLog), n) }

func (h *SystemdHost) Exists(path string) bool {
	_, err := os.Stat(h.hostPath(path))
	return err == nil
}

// FileTail returns the last n bytes of a file, from a line start.
func FileTail(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	off := max(fi.Size()-int64(n), 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(n)))
	if err != nil {
		return nil, err
	}
	if off > 0 {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			data = data[i+1:]
		}
	}
	return data, nil
}
