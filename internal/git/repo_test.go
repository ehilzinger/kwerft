// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"errors"
	"testing"
)

func TestParseRepositoryForms(t *testing.T) {
	same := []string{
		"https://github.com/Acme/API",
		"https://github.com/acme/api.git",
		"https://github.com/acme/api/",
		"git@github.com:acme/api.git",
		"ssh://git@github.com/acme/api.git",
		"ssh://git@GitHub.com:22/acme/api",
		"http://github.com/acme/api",
	}
	for _, s := range same {
		r, err := ParseRepository(s)
		if err != nil {
			t.Errorf("%s: %v", s, err)
			continue
		}
		if r.Key() != "github.com/acme/api" {
			t.Errorf("%s: key %q", s, r.Key())
		}
	}
	r, err := ParseRepository("https://gitlab.example.com/group/sub/proj.git")
	if err != nil || r.Owner() != "group" || r.Name() != "proj" || r.Path != "group/sub/proj" || r.HTTPS() != "https://gitlab.example.com/group/sub/proj.git" {
		t.Errorf("subgroups: %+v %v", r, err)
	}
	r, _ = ParseRepository("ssh://deploy@git.example.com:2222/team/app.git")
	if !r.SSH || r.SSHUser != "deploy" || r.SSHAddress() != "git.example.com:2222" {
		t.Errorf("ssh port: %+v", r)
	}
	r, _ = ParseRepository("git@codeberg.org:me/thing")
	if !r.SSH || r.SSHUser != "git" || r.SSHAddress() != "codeberg.org:22" || r.String() != "https://codeberg.org/me/thing" {
		t.Errorf("scp form: %+v", r)
	}
	if !SameRepository("git@github.com:acme/api.git", "https://github.com/ACME/api") || SameRepository("https://github.com/acme/api", "https://github.com/acme/web") {
		t.Error("SameRepository")
	}
}

func TestParseRepositoryRejects(t *testing.T) {
	for _, s := range []string{
		"", "github.com/acme/api", "https://github.com/acme", "https://github.com/../etc", "ftp://x/y/z",
		"https://github.com/acme/api?x=1", "git@github.com:/", "file:///tmp/repo", "https://github.com/acme/-upload-pack",
		"https://ex ample.com/a/b", "https://github.com/acme/api with space",
	} {
		if _, err := ParseRepository(s); err == nil {
			t.Errorf("%q parsed", s)
		}
	}
	for _, s := range []string{"https://user:ghp_secret@github.com/acme/api", "https://ghp_secret@github.com/acme/api", "ssh://git:pw@host/a/b"} {
		if _, err := ParseRepository(s); !errors.Is(err, ErrCredentialsInURL) {
			t.Errorf("%q: %v, want ErrCredentialsInURL", s, err)
		}
	}
}

func TestShortLineKeepsUTF8(t *testing.T) {
	if got := shortLine("äöü\nrest", 3); got != "ä" {
		t.Errorf("shortLine = %q", got)
	}
}
