// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"bufio"
	"bytes"
	"encoding/json"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Step states in Upgrade.status.steps.
const (
	StepRunning = "Running"
	StepDone    = "Done"
	StepSkipped = "Skipped"
	StepFailed  = "Failed"
)

// progressLine is one line of install.sh --progress FILE: a stage
// {"id","label","state":"ok|skip|fail","detail","at"}, or the last line
// {"exit":<code>}.
type progressLine struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	At     string `json:"at"`
	Exit   *int   `json:"exit"`
}

// Progress is what the installer reported so far.
type Progress struct {
	Steps []kwerftv1.UpgradeStep
	// Exit is the installer's exit code once it wrote one.
	Exit *int
}

// ParseProgress reads a progress file. Lines that are not (yet) complete
// JSON are ignored: the file is read while the installer writes it. A stage
// reported twice keeps its last state.
func ParseProgress(data []byte) Progress {
	var p Progress
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var l progressLine
		if err := json.Unmarshal(line, &l); err != nil {
			continue
		}
		if l.Exit != nil {
			code := *l.Exit
			p.Exit = &code
			continue
		}
		if l.ID == "" {
			continue
		}
		step := kwerftv1.UpgradeStep{ID: l.ID, Label: l.Label, Detail: truncate(l.Detail, 500)}
		switch l.State {
		case "ok":
			step.State = StepDone
		case "skip":
			step.State = StepSkipped
		case "fail":
			step.State = StepFailed
		default:
			step.State = StepRunning
		}
		if step.Label == "" {
			step.Label = l.ID
		}
		if t, err := time.Parse(time.RFC3339, l.At); err == nil {
			step.At = &metav1.Time{Time: t}
		}
		replaced := false
		for i := range p.Steps {
			if p.Steps[i].ID == step.ID {
				p.Steps[i], replaced = step, true
			}
		}
		if !replaced {
			p.Steps = append(p.Steps, step)
		}
	}
	return p
}

// ExitReason maps an installer exit code to Upgrade.status.reason
// (install.sh's EXIT_* contract).
func ExitReason(code int) string {
	switch code {
	case 2:
		return "Usage"
	case 10:
		return "Preflight"
	case 20:
		return "Network"
	case 30:
		return "Kubernetes"
	case 40:
		return "Platform"
	case 50:
		return "Kwerft"
	}
	return "Installer"
}
