// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/git/gittest"
)

const (
	shaMain    = "1111111111111111111111111111111111111111"
	shaFeature = "2222222222222222222222222222222222222222"
)

func fixture(t *testing.T) (*gittest.Server, *git.Factory) {
	t.Helper()
	fake := gittest.New(t)
	fake.AddRepo("acme/api", "main", gittest.Commit{SHA: shaMain, Message: "Fix checkout\n\nBody", Author: "Mara", Time: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)})
	fake.SetBranch("acme/api", "feature/search", gittest.Commit{SHA: shaFeature, Message: "Search", Author: "Sam"})
	priv := fake.AddRepo("acme/secret", "trunk", gittest.Commit{SHA: shaMain, Message: "Init", Author: "Ola"})
	priv.Private = true
	fake.AddRepo("group/sub/app", "main", gittest.Commit{SHA: shaFeature, Message: "Sub", Author: "Ola"})
	return fake, fake.Factory()
}

func conn(fake *gittest.Server, p kwerftv1.GitProvider, auth kwerftv1.GitAuth) git.Connection {
	c := git.Connection{Name: "test", Provider: p, URL: fake.URL, Auth: auth}
	switch auth {
	case kwerftv1.GitAuthToken:
		c.Token = fake.Token
	case kwerftv1.GitAuthGitHubApp:
		c.AppID, c.InstallationID, c.GitHubAppKey = fake.AppID, fake.InstallationID, fake.AppKeyPEM
	}
	return c
}

func repo(t *testing.T, fake *gittest.Server, path string) git.Repo {
	t.Helper()
	r, err := git.ParseRepository(fake.URL + "/" + path + ".git")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestProvidersReadRepositories runs the same reads against every provider
// API and the Git protocol.
func TestProvidersReadRepositories(t *testing.T) {
	fake, f := fixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		conn     git.Connection
		account  string
		messages bool // the API knows commit messages; ls-remote does not
		files    bool
	}{
		{"github token", conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthToken), "builder", true, true},
		{"github app", conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthGitHubApp), "kwerft-test[bot] on acme", true, true},
		{"gitlab", conn(fake, kwerftv1.GitLab, kwerftv1.GitAuthToken), "builder", true, true},
		{"gitea", conn(fake, kwerftv1.Gitea, kwerftv1.GitAuthToken), "builder", true, true},
		{"generic", conn(fake, kwerftv1.Generic, kwerftv1.GitAuthToken), "", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := f.For(c.conn)
			if err != nil {
				t.Fatal(err)
			}
			account, err := p.Verify(ctx)
			if err != nil || account != c.account {
				t.Fatalf("Verify = %q, %v", account, err)
			}
			api := repo(t, fake, "acme/api")
			if def, err := p.DefaultBranch(ctx, api); err != nil || def != "main" {
				t.Errorf("DefaultBranch = %q, %v", def, err)
			}
			head, err := p.Head(ctx, api, "main")
			if err != nil || head.SHA != shaMain {
				t.Fatalf("Head = %+v, %v", head, err)
			}
			if c.messages && (head.Author != "Mara" || !strings.HasPrefix(head.Message, "Fix checkout")) {
				t.Errorf("Head = %+v", head)
			}
			if h, err := p.Head(ctx, api, "feature/search"); err != nil || h.SHA != shaFeature {
				t.Errorf("Head(feature/search) = %+v, %v", h, err)
			}
			if _, err := p.Head(ctx, api, "nope"); !errors.Is(err, git.ErrNotFound) {
				t.Errorf("missing branch: %v", err)
			}
			if cm, err := p.Commit(ctx, api, shaFeature); err != nil || cm.SHA != shaFeature {
				t.Errorf("Commit = %+v, %v", cm, err)
			}
			// Private repositories, and GitLab's subgroups.
			if h, err := p.Head(ctx, repo(t, fake, "acme/secret"), "trunk"); err != nil || h.SHA != shaMain {
				t.Errorf("private Head = %+v, %v", h, err)
			}
			if c.conn.Provider == kwerftv1.GitLab || c.conn.Provider == kwerftv1.Generic {
				if h, err := p.Head(ctx, repo(t, fake, "group/sub/app"), "main"); err != nil || h.SHA != shaFeature {
					t.Errorf("subgroup Head = %+v, %v", h, err)
				}
			}
			found, err := p.FileExists(ctx, api, shaMain, "Dockerfile")
			if c.files {
				if err != nil || !found {
					t.Errorf("Dockerfile = %v, %v", found, err)
				}
				if found, err := p.FileExists(ctx, api, shaMain, "web/Dockerfile"); err != nil || found {
					t.Errorf("web/Dockerfile = %v, %v", found, err)
				}
			} else if !errors.Is(err, git.ErrUnsupported) {
				t.Errorf("generic FileExists: %v", err)
			}
		})
	}
}

func TestPublicAndRejectedCredentials(t *testing.T) {
	fake, f := fixture(t)
	ctx := context.Background()
	public := conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthNone)
	p, _ := f.For(public)
	if h, err := p.Head(ctx, repo(t, fake, "acme/api"), "main"); err != nil || h.SHA != shaMain {
		t.Errorf("public Head = %+v, %v", h, err)
	}
	if _, err := p.Head(ctx, repo(t, fake, "acme/secret"), "trunk"); !errors.Is(err, git.ErrNotFound) {
		t.Errorf("private repo without credentials: %v", err)
	}
	for _, prov := range []kwerftv1.GitProvider{kwerftv1.GitHub, kwerftv1.GitLab, kwerftv1.Gitea} {
		c := conn(fake, prov, kwerftv1.GitAuthToken)
		c.Token = "wrong"
		p, _ := f.For(c)
		if _, err := p.Verify(ctx); !errors.Is(err, git.ErrUnauthorized) {
			t.Errorf("%s wrong token: %v", prov, err)
		} else if strings.Contains(err.Error(), "wrong") {
			t.Errorf("%s error shows the token: %v", prov, err)
		}
	}
	app := conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthGitHubApp)
	app.GitHubAppKey = []byte("-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----\n")
	p, _ = f.For(app)
	if _, err := p.Verify(ctx); !errors.Is(err, git.ErrUnauthorized) {
		t.Errorf("bad App key: %v", err)
	}
	generic := conn(fake, kwerftv1.Generic, kwerftv1.GitAuthToken)
	generic.Token = "wrong"
	p, _ = f.For(generic)
	if _, err := p.Head(ctx, repo(t, fake, "acme/secret"), "trunk"); !errors.Is(err, git.ErrUnauthorized) {
		t.Errorf("ls-remote with a wrong token: %v", err)
	}
}

func TestWebhooksAreCreatedOnceAndUpdatedOnRequest(t *testing.T) {
	fake, f := fixture(t)
	ctx := context.Background()
	hook := git.Hook{URL: "https://console.example.com/api/v1/hooks/git/test", Secret: "one"}
	for _, prov := range []kwerftv1.GitProvider{kwerftv1.GitHub, kwerftv1.GitLab, kwerftv1.Gitea} {
		p, _ := f.For(conn(fake, prov, kwerftv1.GitAuthToken))
		r := repo(t, fake, "acme/api")
		if handled, err := p.ConnectionWebhook(ctx, hook); handled || err != nil {
			t.Errorf("%s: token connections have no connection-wide hook: %v %v", prov, handled, err)
		}
		for range 2 {
			if err := p.EnsureWebhook(ctx, r, hook, false); err != nil {
				t.Fatalf("%s EnsureWebhook: %v", prov, err)
			}
		}
		if err := p.EnsureWebhook(ctx, r, git.Hook{URL: hook.URL, Secret: "two"}, true); err != nil {
			t.Fatalf("%s update: %v", prov, err)
		}
	}
	hooks := fake.Hooks()
	if len(hooks) != 3 {
		t.Fatalf("hooks = %+v, want one per provider", hooks)
	}
	for _, h := range hooks {
		if h.URL != hook.URL || h.Secret != "two" {
			t.Errorf("hook = %+v", h)
		}
		if h.Provider != "gitlab" && !slices.Contains(h.Events, "pull_request") {
			t.Errorf("events = %v", h.Events)
		}
	}

	// A GitHub App sets its own webhook.
	p, _ := f.For(conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthGitHubApp))
	if handled, err := p.ConnectionWebhook(ctx, hook); !handled || err != nil {
		t.Fatalf("App hook: %v %v", handled, err)
	}
	if hs := fake.Hooks(); hs[len(hs)-1].Provider != "app" || hs[len(hs)-1].Secret != "one" {
		t.Errorf("App hook = %+v", hs[len(hs)-1])
	}

	// Without the permission, hosts say 403.
	fake.DenyHooks(true)
	p, _ = f.For(conn(fake, kwerftv1.Gitea, kwerftv1.GitAuthToken))
	if err := p.EnsureWebhook(ctx, repo(t, fake, "acme/secret"), hook, false); !errors.Is(err, git.ErrForbidden) {
		t.Errorf("denied: %v", err)
	}
	// Deploy keys and generic hosts cannot.
	p, _ = f.For(conn(fake, kwerftv1.Generic, kwerftv1.GitAuthToken))
	if err := p.EnsureWebhook(ctx, repo(t, fake, "acme/api"), hook, false); !errors.Is(err, git.ErrUnsupported) {
		t.Errorf("generic: %v", err)
	}
}

func TestReportStatus(t *testing.T) {
	fake, f := fixture(t)
	ctx := context.Background()
	r := repo(t, fake, "acme/api")
	st := git.Status{SHA: shaMain, Context: "kwerft/shop/api", Description: "Build #3 is queued", TargetURL: "https://console.example.com/apps/shop/api?build=api-111-x", ExternalID: "shop/api-111-x"}
	for _, prov := range []kwerftv1.GitProvider{kwerftv1.GitHub, kwerftv1.GitLab, kwerftv1.Gitea} {
		p, _ := f.For(conn(fake, prov, kwerftv1.GitAuthToken))
		for _, state := range []git.State{git.StatePending, git.StateRunning, git.StateSuccess} {
			s := st
			s.State = state
			if _, err := p.ReportStatus(ctx, r, s); err != nil {
				t.Errorf("%s %s: %v", prov, state, err)
			}
		}
		// GitLab refuses the same state twice; a retried report is fine.
		s := st
		s.State = git.StateSuccess
		if _, err := p.ReportStatus(ctx, r, s); err != nil {
			t.Errorf("%s repeated: %v", prov, err)
		}
	}
	var got []string
	for _, s := range fake.Statuses() {
		got = append(got, s.Provider+":"+s.State)
		if s.Context != st.Context || s.TargetURL != st.TargetURL {
			t.Errorf("status = %+v", s)
		}
	}
	want := []string{"github:pending", "github:pending", "github:success", "github:success",
		"gitlab:pending", "gitlab:running", "gitlab:success",
		"gitea:pending", "gitea:pending", "gitea:success", "gitea:success"}
	if !slices.Equal(got, want) {
		t.Errorf("statuses = %v\nwant %v", got, want)
	}

	// A GitHub App reports one check run and updates it.
	p, _ := f.For(conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthGitHubApp))
	st.State = git.StatePending
	ref, err := p.ReportStatus(ctx, r, st)
	if err != nil || ref == "" {
		t.Fatalf("check run: %q %v", ref, err)
	}
	st.State, st.Ref = git.StateFailure, ref
	if ref2, err := p.ReportStatus(ctx, r, st); err != nil || ref2 != ref {
		t.Fatalf("update: %q %v", ref2, err)
	}
	all := fake.Statuses()
	last := all[len(all)-1]
	if last.Provider != "github-check" || last.State != "completed" || last.Conclusion != "failure" || last.SHA != shaMain {
		t.Errorf("check run = %+v", last)
	}
	// Tokens cannot create check runs; deploy keys report nothing.
	p, _ = f.For(git.Connection{Provider: kwerftv1.GitHub, URL: fake.URL, Auth: kwerftv1.GitAuthSSHKey})
	if _, err := p.ReportStatus(ctx, r, st); !errors.Is(err, git.ErrUnsupported) {
		t.Errorf("deploy key: %v", err)
	}
}

func TestInstallationTokenIsCached(t *testing.T) {
	fake, f := fixture(t)
	ctx := context.Background()
	p, _ := f.For(conn(fake, kwerftv1.GitHub, kwerftv1.GitAuthGitHubApp))
	for range 3 {
		if _, err := p.Head(ctx, repo(t, fake, "acme/api"), "main"); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, req := range fake.Requests() {
		if strings.HasSuffix(req, "/access_tokens") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("installation tokens fetched %d times, want 1", n)
	}
}
