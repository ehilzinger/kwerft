// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func mustWindow(t *testing.T, w *kwerftv1.MaintenanceWindow) *Window {
	t.Helper()
	pw, err := ParseWindow(w)
	if err != nil {
		t.Fatal(err)
	}
	return pw
}

func TestWindowOpen(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	w := mustWindow(t, &kwerftv1.MaintenanceWindow{Days: []string{"Sun"}, Start: "03:00",
		Duration: &metav1.Duration{Duration: 2 * time.Hour}, TimeZone: "Europe/Berlin"})
	sun := func(h, m int) time.Time { return time.Date(2026, 10, 11, h, m, 0, 0, berlin) } // a Sunday
	for _, c := range []struct {
		at   time.Time
		open bool
	}{
		{sun(2, 59), false}, {sun(3, 0), true}, {sun(4, 59), true}, {sun(5, 0), false},
		{sun(3, 30).AddDate(0, 0, 1), false}, // Monday
		{sun(3, 30).UTC(), true},             // the zone is the window's, not the clock's
	} {
		open, end := w.Open(c.at)
		if open != c.open {
			t.Errorf("%s: open = %v", c.at, open)
		}
		if open && !end.Equal(sun(5, 0)) {
			t.Errorf("%s: end = %s", c.at, end)
		}
	}
	if next := w.Next(sun(5, 0)); !next.Equal(sun(3, 0).AddDate(0, 0, 7)) {
		t.Errorf("next = %s", next)
	}
	if next := w.Next(sun(1, 0)); !next.Equal(sun(3, 0)) {
		t.Errorf("next = %s", next)
	}
}

func TestWindowAcrossMidnight(t *testing.T) {
	w := mustWindow(t, &kwerftv1.MaintenanceWindow{Days: []string{"Saturday"}, Start: "23:00", Duration: &metav1.Duration{Duration: 3 * time.Hour}})
	sat := time.Date(2026, 10, 10, 23, 30, 0, 0, time.UTC)
	if open, end := w.Open(sat.Add(2 * time.Hour)); !open || !end.Equal(time.Date(2026, 10, 11, 2, 0, 0, 0, time.UTC)) {
		t.Errorf("Sunday 01:30 after a Saturday 23:00 start: open %v end %s", open, end)
	}
	every := mustWindow(t, &kwerftv1.MaintenanceWindow{Start: "12:00"})
	if open, _ := every.Open(time.Date(2026, 10, 7, 13, 59, 0, 0, time.UTC)); !open {
		t.Error("no days means every day, two hours by default")
	}
}

func TestWindowInvalid(t *testing.T) {
	for _, w := range []*kwerftv1.MaintenanceWindow{
		{Start: "3am"}, {Start: "25:00"}, {Start: "03:00", Days: []string{"Funday"}},
		{Start: "03:00", TimeZone: "Mars/Olympus"}, {Start: "03:00", Duration: &metav1.Duration{Duration: 48 * time.Hour}},
	} {
		if _, err := ParseWindow(w); err == nil {
			t.Errorf("%+v parsed", w)
		}
	}
	var none *Window
	if open, _ := none.Open(time.Now()); open || !none.Next(time.Now()).IsZero() {
		t.Error("no window is never open")
	}
}

func TestParseProgress(t *testing.T) {
	data := []byte(`{"id":"preflight","label":"Preflight","state":"ok","detail":"Ubuntu 24.04","at":"2026-10-05T10:00:00Z"}
{"id":"system","label":"System","state":"skip","detail":"","at":"2026-10-05T10:00:01Z"}
not json
{"id":"kwerft","label":"Kwerft","state":"fail","detail":"helm failed","at":"2026-10-05T10:05:00Z"}
{"exit":50}
{"id":"half`)
	p := ParseProgress(data)
	if len(p.Steps) != 3 || p.Exit == nil || *p.Exit != 50 {
		t.Fatalf("progress = %+v", p)
	}
	if p.Steps[0].State != StepDone || p.Steps[1].State != StepSkipped || p.Steps[2].State != StepFailed || p.Steps[2].Detail != "helm failed" {
		t.Errorf("steps = %+v", p.Steps)
	}
	if ExitReason(50) != "Kwerft" || ExitReason(30) != "Kubernetes" || ExitReason(1) != "Installer" {
		t.Error("exit reasons")
	}
}
