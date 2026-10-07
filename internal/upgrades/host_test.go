// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseUnitState(t *testing.T) {
	for out, want := range map[string]UnitState{
		"LoadState=not-found\nActiveState=inactive\nSubState=dead\nExecMainStatus=0\n":                                   {},
		"LoadState=loaded\nActiveState=active\nSubState=running\nExecMainStatus=0\nExecMainExitTimestampMonotonic=0\n":   {Exists: true, Running: true},
		"LoadState=loaded\nActiveState=active\nSubState=exited\nExecMainStatus=0\nExecMainExitTimestampMonotonic=123\n":  {Exists: true, Finished: true},
		"LoadState=loaded\nActiveState=failed\nSubState=failed\nExecMainStatus=50\nExecMainExitTimestampMonotonic=123\n": {Exists: true, Finished: true, ExitCode: 50},
		"LoadState=loaded\nActiveState=failed\nSubState=failed\nExecMainStatus=0\nExecMainExitTimestampMonotonic=123\n":  {Exists: true, Finished: true, ExitCode: 1},
		"LoadState=loaded\nActiveState=inactive\nSubState=dead\nExecMainStatus=0\nExecMainExitTimestampMonotonic=0\n":    {},
		"LoadState=loaded\nActiveState=activating\nSubState=start\nExecMainStatus=0\nExecMainExitTimestampMonotonic=0\n": {Exists: true, Running: true},
		"LoadState=loaded\nActiveState=inactive\nSubState=dead\nExecMainStatus=20\nExecMainExitTimestampMonotonic=99\n":  {Exists: true, Finished: true, ExitCode: 20},
	} {
		if got := ParseUnitState(out); got != want {
			t.Errorf("%q: %+v, want %+v", out, got, want)
		}
	}
}

func TestSystemdHostCommands(t *testing.T) {
	var calls [][]string
	h := &SystemdHost{Root: "/host", Exec: func(_ context.Context, root string, args ...string) (string, error) {
		if root != "/host" {
			t.Errorf("root %s", root)
		}
		calls = append(calls, args)
		return "LoadState=loaded\nActiveState=active\nSubState=running\n", nil
	}}
	ctx := context.Background()
	if _, err := h.Run(ctx, Command{Unit: "kwerft-upgrade-x-helm", Env: []string{"KUBECONFIG=" + HostKubeconfig}, Args: []string{HostHelm, "list"}}); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(ctx, Command{Unit: "kwerft-upgrade-x", Args: []string{"/var/lib/kwerft/upgrade/x/install.sh", "--yes"}}); err != nil {
		t.Fatal(err)
	}
	if st, err := h.Unit(ctx, "kwerft-upgrade-x"); err != nil || !st.Running {
		t.Fatalf("unit %+v %v", st, err)
	}
	run, start := calls[0], calls[1]
	if run[0] != "/usr/bin/systemd-run" || !slices.Contains(run, "--wait") || !slices.Contains(run, "--pipe") ||
		!strings.HasPrefix(run[1], "--unit=kwerft-upgrade-x-helm-") || !slices.Contains(run, "--setenv=KUBECONFIG="+HostKubeconfig) {
		t.Errorf("run = %v", run)
	}
	if i := slices.Index(run, "--"); i < 0 || !slices.Equal(run[i+1:], []string{HostHelm, "list"}) {
		t.Errorf("run's command = %v", run)
	}
	// The installer's unit is started by a systemd-run on the host, in a
	// unit the runner waits for: one that neither waits nor pipes cannot
	// reach systemd from the pod.
	if start[0] != "/usr/bin/systemd-run" || !slices.Contains(start, "--wait") || !slices.Contains(start, "--pipe") ||
		!strings.HasPrefix(start[1], "--unit=kwerft-upgrade-x-start-") {
		t.Errorf("start = %v", start)
	}
	inner := start[slices.Index(start, "--")+1:]
	if inner[0] != "/usr/bin/systemd-run" || !slices.Contains(inner, "--unit=kwerft-upgrade-x") ||
		!slices.Contains(inner, "--property=RemainAfterExit=yes") || !slices.Contains(inner, "--property=KillMode=process") ||
		slices.Contains(inner, "--wait") || slices.Contains(inner, "--pipe") {
		t.Errorf("installer unit = %v", inner)
	}
	if i := slices.Index(inner, "--"); i < 0 || !slices.Equal(inner[i+1:], []string{"/var/lib/kwerft/upgrade/x/install.sh", "--yes"}) {
		t.Errorf("installer command = %v", inner)
	}
	// systemctl runs on the host as a unit too, never in the chroot.
	show := calls[2]
	if show[0] != "/usr/bin/systemd-run" || !slices.Contains(show, "--pipe") || !strings.HasPrefix(show[1], "--unit=kwerft-upgrade-x-show-") {
		t.Errorf("show = %v", show)
	}
	if i := slices.Index(show, "--"); i < 0 || !slices.Equal(show[i+1:i+4], []string{"/usr/bin/systemctl", "show", "kwerft-upgrade-x.service"}) {
		t.Errorf("show's command = %v", show)
	}
	calls = nil
	if err := h.Remove(ctx, "kwerft-upgrade-x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if c[0] != "/usr/bin/systemd-run" {
			t.Errorf("remove ran %v in the chroot", c)
		}
	}
}

func TestFileTail(t *testing.T) {
	p := filepath.Join(t.TempDir(), "install.log")
	if err := os.WriteFile(p, []byte("first line\nsecond line\nthird line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := FileTail(p, 15)
	if err != nil || string(got) != "third line\n" {
		t.Errorf("tail = %q %v", got, err)
	}
	if got, _ := FileTail(p, 1000); !strings.HasPrefix(string(got), "first") {
		t.Errorf("whole file = %q", got)
	}
}
