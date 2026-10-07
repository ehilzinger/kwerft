// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const hookSecret = "s3cret-webhook"

func headers(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	sig := Sign(body, hookSecret)
	cases := []struct {
		name string
		p    kwerftv1.GitProvider
		h    http.Header
		body []byte
		want bool
	}{
		{"github ok", kwerftv1.GitHub, headers("X-Hub-Signature-256", "sha256="+sig), body, true},
		{"github wrong secret", kwerftv1.GitHub, headers("X-Hub-Signature-256", "sha256="+Sign(body, "other")), body, false},
		{"github tampered body", kwerftv1.GitHub, headers("X-Hub-Signature-256", "sha256="+sig), []byte(`{"ref":"refs/heads/evil"}`), false},
		{"github no prefix", kwerftv1.GitHub, headers("X-Hub-Signature-256", sig), body, false},
		{"github testSHA only", kwerftv1.GitHub, headers("X-Hub-Signature", "testSHA=abc"), body, false},
		{"github unsigned", kwerftv1.GitHub, headers(), body, false},
		{"github gitlab token does not count", kwerftv1.GitHub, headers("X-Gitlab-Token", hookSecret), body, false},
		{"gitlab ok", kwerftv1.GitLab, headers("X-Gitlab-Token", hookSecret), body, true},
		{"gitlab wrong", kwerftv1.GitLab, headers("X-Gitlab-Token", hookSecret+"x"), body, false},
		{"gitlab hub signature does not count", kwerftv1.GitLab, headers("X-Hub-Signature-256", "sha256="+sig), body, false},
		{"gitea ok", kwerftv1.Gitea, headers("X-Gitea-Signature", sig), body, true},
		{"forgejo ok", kwerftv1.Gitea, headers("X-Forgejo-Signature", sig), body, true},
		{"gitea hub header", kwerftv1.Gitea, headers("X-Hub-Signature-256", "sha256="+sig), body, true},
		{"gitea wrong", kwerftv1.Gitea, headers("X-Gitea-Signature", strings.Repeat("0", 64)), body, false},
		{"gitea garbage", kwerftv1.Gitea, headers("X-Gitea-Signature", "zz"), body, false},
		{"generic gitlab style", kwerftv1.Generic, headers("X-Gitlab-Token", hookSecret), body, true},
		{"generic hmac", kwerftv1.Generic, headers("X-Hub-Signature-256", "sha256="+sig), body, true},
		{"generic unsigned", kwerftv1.Generic, headers(), body, false},
	}
	for _, c := range cases {
		err := VerifySignature(c.p, c.h, c.body, hookSecret)
		if (err == nil) != c.want {
			t.Errorf("%s: err = %v, want ok=%v", c.name, err, c.want)
		}
	}
	// No secret stored: nothing verifies, not even an empty token.
	if VerifySignature(kwerftv1.GitLab, headers("X-Gitlab-Token", ""), body, "") == nil {
		t.Error("empty secret verified")
	}
}

const testSHA = "4f2c1ab9d0e5c3b2a1908f7e6d5c4b3a29180716"

func TestParseGitHubPush(t *testing.T) {
	body := `{"ref":"refs/heads/main","after":"` + testSHA + `","deleted":false,
	  "repository":{"id":1,"full_name":"acme/api","clone_url":"https://github.com/acme/api.git","ssh_url":"git@github.com:acme/api.git","html_url":"https://github.com/acme/api"},
	  "head_commit":{"id":"` + testSHA + `","message":"Fix checkout\n\nlong body","timestamp":"2026-10-04T10:00:00Z","author":{"name":"Mara"}},
	  "pusher":{"name":"mara"},"sender":{"login":"mara-l"}}`
	ev, err := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "push"), []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventPush || ev.Branch != "main" || ev.Commit.SHA != testSHA || ev.Commit.Author != "Mara" || ev.Sender != "mara-l" ||
		!strings.HasPrefix(ev.Commit.Message, "Fix checkout") || len(ev.Repositories) != 3 || ev.Deleted || ev.Tag {
		t.Errorf("event = %+v", ev)
	}
	del := `{"ref":"refs/heads/old","after":"0000000000000000000000000000000000000000","deleted":true,"repository":{"full_name":"acme/api"}}`
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "push"), []byte(del)); !ev.Deleted {
		t.Errorf("deletion = %+v", ev)
	}
	tag := `{"ref":"refs/tags/v1","after":"` + testSHA + `","repository":{"full_name":"acme/api"}}`
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "push"), []byte(tag)); !ev.Tag {
		t.Errorf("tag = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "ping"), []byte(`{}`)); ev.Kind != EventPing {
		t.Errorf("ping = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "issues"), []byte(`{}`)); ev.Kind != EventOther {
		t.Errorf("issues = %+v", ev)
	}
}

func prBody(action string, headRepo string) string {
	head := `null`
	if headRepo != "" {
		id := "1"
		if headRepo != "acme/api" {
			id = "99"
		}
		head = `{"id":` + id + `,"full_name":"` + headRepo + `"}`
	}
	return `{"action":"` + action + `","number":7,"pull_request":{"number":7,"title":"Add search","user":{"login":"sam"},
	  "head":{"ref":"feature","sha":"` + testSHA + `","repo":` + head + `},
	  "base":{"ref":"main","repo":{"id":1,"full_name":"acme/api","clone_url":"https://github.com/acme/api.git"}}},
	  "sender":{"login":"sam"}}`
}

func TestParseGitHubPullRequest(t *testing.T) {
	ev, err := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "pull_request"), []byte(prBody("synchronize", "acme/api")))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventPullRequest || !ev.Build || ev.Fork || ev.PullRequest != 7 || ev.Branch != "main" || ev.HeadBranch != "feature" ||
		ev.Commit.SHA != testSHA || ev.Commit.Message != "Add search" {
		t.Errorf("same-repo PR = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "pull_request"), []byte(prBody("opened", "mallory/api"))); !ev.Fork {
		t.Errorf("fork PR = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "pull_request"), []byte(prBody("opened", ""))); !ev.Fork {
		t.Errorf("deleted-fork PR = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitHub, headers("X-GitHub-Event", "pull_request"), []byte(prBody("labeled", "acme/api"))); ev.Build {
		t.Errorf("labeled PR builds: %+v", ev)
	}
	// Gitea says "synchronized" and identifies itself.
	if ev, _ := ParseEvent(kwerftv1.Gitea, headers("X-Gitea-Event", "pull_request", "X-GitHub-Event", "pull_request"), []byte(prBody("synchronized", "acme/api"))); !ev.Build || ev.Fork {
		t.Errorf("gitea PR = %+v", ev)
	}
}

func TestParseGitLab(t *testing.T) {
	push := `{"object_kind":"push","ref":"refs/heads/main","after":"` + testSHA + `","user_username":"mara",
	  "project":{"id":5,"git_http_url":"https://gitlab.example.com/group/sub/app.git","git_ssh_url":"git@gitlab.example.com:group/sub/app.git","web_url":"https://gitlab.example.com/group/sub/app"},
	  "commits":[{"id":"` + testSHA + `","message":"Ship it\n","timestamp":"2026-10-04T10:00:00+02:00","author":{"name":"Mara"}}]}`
	ev, err := ParseEvent(kwerftv1.GitLab, headers("X-Gitlab-Event", "Push Hook"), []byte(push))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != EventPush || ev.Branch != "main" || ev.Sender != "mara" || ev.Commit.Author != "Mara" || len(ev.Repositories) != 3 {
		t.Errorf("push = %+v", ev)
	}
	mr := func(action, oldrev string, source, target int) string {
		return `{"object_kind":"merge_request","user":{"username":"sam"},"project":{"id":5,"git_http_url":"https://gitlab.example.com/group/sub/app.git"},
		  "object_attributes":{"iid":3,"action":"` + action + `","oldrev":"` + oldrev + `","title":"MR","source_branch":"feat","target_branch":"main",
		  "source_project_id":` + itoa(source) + `,"target_project_id":` + itoa(target) + `,"last_commit":{"id":"` + testSHA + `","message":"m","author":{"name":"Sam"}}}}`
	}
	if ev, _ := ParseEvent(kwerftv1.GitLab, headers("X-Gitlab-Event", "Merge Request Hook"), []byte(mr("open", "", 5, 5))); !ev.Build || ev.Fork || ev.PullRequest != 3 {
		t.Errorf("open MR = %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitLab, headers("X-Gitlab-Event", "Merge Request Hook"), []byte(mr("update", "", 5, 5))); ev.Build {
		t.Errorf("title edit builds: %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitLab, headers("X-Gitlab-Event", "Merge Request Hook"), []byte(mr("update", testSHA, 5, 5))); !ev.Build {
		t.Errorf("new commits do not build: %+v", ev)
	}
	if ev, _ := ParseEvent(kwerftv1.GitLab, headers("X-Gitlab-Event", "Merge Request Hook"), []byte(mr("open", "", 9, 5))); !ev.Fork {
		t.Errorf("fork MR = %+v", ev)
	}
	// A GitHub-style event to a GitLab connection is not read as one.
	if ev, _ := ParseEvent(kwerftv1.GitLab, headers("X-GitHub-Event", "push"), []byte(`{}`)); ev.Kind != EventOther {
		t.Errorf("cross-provider = %+v", ev)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
