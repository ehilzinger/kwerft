// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package importplan

import (
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	for in, want := range map[string]string{
		"web":                   "web",
		"Legacy_App":            "legacy-app",
		"my.service--1":         "my-service-1",
		"3d":                    "app-3d",
		"__":                    "app",
		"-x-":                   "x",
		strings.Repeat("a", 70): strings.Repeat("a", MaxAppName),
	} {
		got := SafeName(in, MaxAppName, "app")
		if got != want || !ValidAppName(got) {
			t.Errorf("SafeName(%q) = %q, want %q (valid)", in, got, want)
		}
	}
	if got := SafeName(strings.Repeat("ab-", 30), 10, "app"); got != "ab-ab-ab-a" {
		t.Errorf("truncated %q", got)
	}
}

func TestUnique(t *testing.T) {
	taken := map[string]bool{"web": true, "web-2": true}
	if got := Unique("web", 63, func(n string) bool { return taken[n] }); got != "web-3" {
		t.Errorf("Unique = %q, want web-3", got)
	}
	long := strings.Repeat("x", 10)
	if got := Unique(long, 10, func(n string) bool { return n == long }); got != "xxxxxxxx-2" {
		t.Errorf("Unique = %q, want it within 10", got)
	}
}
