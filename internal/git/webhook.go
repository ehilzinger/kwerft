// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// ErrBadSignature: the delivery is not signed with the connection's webhook
// secret (or not signed at all).
var ErrBadSignature = errors.New("webhook signature missing or wrong")

// VerifySignature checks a webhook delivery against the connection's
// secret in constant time:
//   - GitHub: X-Hub-Signature-256 = "sha256=" + hex HMAC-SHA256(body)
//   - GitLab: X-Gitlab-Token = the secret itself
//   - Gitea/Forgejo: X-Gitea-Signature / X-Forgejo-Signature = hex
//     HMAC-SHA256(body); newer versions also send X-Hub-Signature-256
//   - generic: any of these
func VerifySignature(p kwerftv1.GitProvider, h http.Header, body []byte, secret string) error {
	if secret == "" {
		return ErrBadSignature
	}
	hubSig := func() bool {
		sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		return ok && hmacMatches(sig, body, secret)
	}
	giteaSig := func() bool {
		for _, name := range []string{"X-Forgejo-Signature", "X-Gitea-Signature"} {
			if sig := h.Get(name); sig != "" {
				return hmacMatches(sig, body, secret)
			}
		}
		return hubSig()
	}
	gitlabToken := func() bool {
		tok := h.Get("X-Gitlab-Token")
		if tok == "" {
			return false
		}
		// Compare digests, so neither the length nor the content leaks.
		a, b := sha256.Sum256([]byte(tok)), sha256.Sum256([]byte(secret))
		return subtle.ConstantTimeCompare(a[:], b[:]) == 1
	}
	var ok bool
	switch p {
	case kwerftv1.GitHub:
		ok = hubSig()
	case kwerftv1.GitLab:
		ok = gitlabToken()
	case kwerftv1.Gitea:
		ok = giteaSig()
	default:
		ok = gitlabToken() || giteaSig()
	}
	if !ok {
		return ErrBadSignature
	}
	return nil
}

func hmacMatches(sigHex string, body []byte, secret string) bool {
	got, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// Sign is the GitHub/Gitea signature of body, for tests and fakes.
func Sign(body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// EventKind is what a webhook delivery is about.
type EventKind string

const (
	EventPush        EventKind = "push"
	EventPullRequest EventKind = "pull-request"
	EventPing        EventKind = "ping"
	EventOther       EventKind = "other"
)

// Event is a webhook delivery in a provider-neutral form.
type Event struct {
	Kind EventKind
	// Repository URLs of the repository the event happened in (for pull
	// requests: the target repository), in every form the host sent.
	Repositories []string
	// Branch pushed to, or the pull request's target branch.
	Branch string
	// Commit to build: the pushed head, or the pull request's head.
	Commit Commit
	// Sender is the account that pushed or opened the pull request.
	Sender string
	// Deleted: a branch deletion push (nothing to build).
	Deleted bool
	// Tag: a push of a tag, not a branch.
	Tag bool

	PullRequest int64
	// Action is the pull request action as the host names it.
	Action string
	// Build: a pull request event with new code (opened, new commits,
	// reopened), as opposed to edits, labels or closing.
	Build bool
	// Fork: the pull request's head is in another repository.
	Fork bool
	// HeadBranch is the pull request's source branch.
	HeadBranch string
}

// ParseEvent reads a verified delivery. Events Kwerft does not act on parse
// as EventOther without an error.
func ParseEvent(p kwerftv1.GitProvider, h http.Header, body []byte) (Event, error) {
	switch {
	case h.Get("X-Gitlab-Event") != "" && (p == kwerftv1.GitLab || p == kwerftv1.Generic):
		return parseGitLab(h.Get("X-Gitlab-Event"), body)
	case p == kwerftv1.Gitea || (p == kwerftv1.Generic && (h.Get("X-Gitea-Event") != "" || h.Get("X-Forgejo-Event") != "")):
		ev := h.Get("X-Forgejo-Event")
		if ev == "" {
			ev = h.Get("X-Gitea-Event")
		}
		if ev == "" {
			ev = h.Get("X-GitHub-Event")
		}
		return parseGitHubLike(ev, body, true)
	case h.Get("X-GitHub-Event") != "" && (p == kwerftv1.GitHub || p == kwerftv1.Generic):
		return parseGitHubLike(h.Get("X-GitHub-Event"), body, false)
	}
	return Event{Kind: EventOther}, nil
}

const zeroSHA = "0000000000000000000000000000000000000000"

type ghRepo struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
	CloneURL string `json:"clone_url"`
	SSHURL   string `json:"ssh_url"`
	HTMLURL  string `json:"html_url"`
}

func (r *ghRepo) urls() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, u := range []string{r.CloneURL, r.SSHURL, r.HTMLURL} {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

type ghUser struct {
	Login    string `json:"login"`
	Username string `json:"username"` // Gitea
	Name     string `json:"name"`
}

func (u ghUser) name() string {
	for _, s := range []string{u.Login, u.Username, u.Name} {
		if s != "" {
			return s
		}
	}
	return ""
}

// parseGitHubLike reads GitHub's payloads, which Gitea and Forgejo copy.
func parseGitHubLike(event string, body []byte, gitea bool) (Event, error) {
	switch event {
	case "ping":
		return Event{Kind: EventPing}, nil
	case "push":
		var p struct {
			Ref        string  `json:"ref"`
			After      string  `json:"after"`
			Deleted    bool    `json:"deleted"`
			Repository *ghRepo `json:"repository"`
			HeadCommit *struct {
				ID        string    `json:"id"`
				Message   string    `json:"message"`
				Timestamp time.Time `json:"timestamp"`
				Author    ghUser    `json:"author"`
			} `json:"head_commit"`
			Pusher ghUser `json:"pusher"`
			Sender ghUser `json:"sender"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("push payload: %w", err)
		}
		e := Event{Kind: EventPush, Repositories: p.Repository.urls(), Commit: Commit{SHA: p.After},
			Deleted: p.Deleted || p.After == zeroSHA}
		e.Sender = p.Sender.name()
		if e.Sender == "" {
			e.Sender = p.Pusher.name()
		}
		branchOrTag(&e, p.Ref)
		if hc := p.HeadCommit; hc != nil && hc.ID == p.After {
			e.Commit = Commit{SHA: hc.ID, Message: hc.Message, Author: hc.Author.name(), Time: hc.Timestamp}
		}
		return e, nil
	case "pull_request", "pull_request_sync":
		var p struct {
			Action      string `json:"action"`
			Number      int64  `json:"number"`
			PullRequest struct {
				Number int64  `json:"number"`
				Title  string `json:"title"`
				User   ghUser `json:"user"`
				Head   struct {
					Ref  string  `json:"ref"`
					SHA  string  `json:"sha"`
					Repo *ghRepo `json:"repo"`
				} `json:"head"`
				Base struct {
					Ref  string  `json:"ref"`
					Repo *ghRepo `json:"repo"`
				} `json:"base"`
			} `json:"pull_request"`
			Repository *ghRepo `json:"repository"`
			Sender     ghUser  `json:"sender"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("pull request payload: %w", err)
		}
		pr := p.PullRequest
		e := Event{Kind: EventPullRequest, Action: p.Action, Branch: pr.Base.Ref, HeadBranch: pr.Head.Ref,
			PullRequest: pr.Number, Sender: p.Sender.name(),
			Commit: Commit{SHA: pr.Head.SHA, Message: pr.Title, Author: pr.User.name()}}
		if e.PullRequest == 0 {
			e.PullRequest = p.Number
		}
		base := pr.Base.Repo
		if base == nil {
			base = p.Repository
		}
		e.Repositories = base.urls()
		// A fork: the head lives in another repository, or in one that is
		// gone (a deleted fork shows as null).
		e.Fork = pr.Head.Repo == nil || base == nil || !sameGHRepo(pr.Head.Repo, base)
		switch p.Action {
		case "opened", "reopened", "synchronize", "synchronized":
			e.Build = true
		}
		if event == "pull_request_sync" {
			e.Build = true
		}
		return e, nil
	}
	return Event{Kind: EventOther}, nil
}

func sameGHRepo(a, b *ghRepo) bool {
	if a.ID != 0 && b.ID != 0 {
		return a.ID == b.ID
	}
	return a.FullName != "" && strings.EqualFold(a.FullName, b.FullName)
}

func branchOrTag(e *Event, ref string) {
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		e.Branch = b
	} else {
		e.Tag = true
	}
}

type glProject struct {
	ID         int64  `json:"id"`
	GitHTTPURL string `json:"git_http_url"`
	GitSSHURL  string `json:"git_ssh_url"`
	WebURL     string `json:"web_url"`
}

func (p glProject) urls() []string {
	var out []string
	for _, u := range []string{p.GitHTTPURL, p.GitSSHURL, p.WebURL} {
		if u != "" {
			out = append(out, u)
		}
	}
	return out
}

type glCommit struct {
	ID        string    `json:"id"`
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`
	Author    struct {
		Name string `json:"name"`
	} `json:"author"`
}

// parseGitLab reads "Push Hook" and "Merge Request Hook" payloads
// (docs.gitlab.com/user/project/integrations/webhook_events).
func parseGitLab(event string, body []byte) (Event, error) {
	switch event {
	case "Push Hook":
		var p struct {
			Ref          string     `json:"ref"`
			After        string     `json:"after"`
			UserUsername string     `json:"user_username"`
			Project      glProject  `json:"project"`
			Commits      []glCommit `json:"commits"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("push payload: %w", err)
		}
		e := Event{Kind: EventPush, Repositories: p.Project.urls(), Commit: Commit{SHA: p.After}, Sender: p.UserUsername,
			Deleted: p.After == zeroSHA}
		branchOrTag(&e, p.Ref)
		for _, c := range p.Commits {
			if c.ID == p.After {
				e.Commit = Commit{SHA: c.ID, Message: c.Message, Author: c.Author.Name, Time: c.Timestamp}
			}
		}
		return e, nil
	case "Merge Request Hook":
		var p struct {
			User struct {
				Username string `json:"username"`
			} `json:"user"`
			Project          glProject `json:"project"`
			ObjectAttributes struct {
				IID             int64     `json:"iid"`
				Action          string    `json:"action"`
				OldRev          string    `json:"oldrev"`
				Title           string    `json:"title"`
				SourceBranch    string    `json:"source_branch"`
				TargetBranch    string    `json:"target_branch"`
				SourceProjectID int64     `json:"source_project_id"`
				TargetProjectID int64     `json:"target_project_id"`
				LastCommit      *glCommit `json:"last_commit"`
			} `json:"object_attributes"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return Event{}, fmt.Errorf("merge request payload: %w", err)
		}
		a := p.ObjectAttributes
		e := Event{Kind: EventPullRequest, Repositories: p.Project.urls(), Branch: a.TargetBranch, HeadBranch: a.SourceBranch,
			PullRequest: a.IID, Action: a.Action, Sender: p.User.Username, Fork: a.SourceProjectID != a.TargetProjectID}
		if c := a.LastCommit; c != nil {
			e.Commit = Commit{SHA: c.ID, Message: c.Message, Author: c.Author.Name, Time: c.Timestamp}
		}
		switch a.Action {
		case "open", "reopen":
			e.Build = true
		case "update":
			e.Build = a.OldRev != "" // new commits; other updates are edits
		}
		return e, nil
	}
	return Event{Kind: EventOther}, nil
}

// DeliveryID is the host's ID for a delivery, for logs and the audit log.
func DeliveryID(h http.Header) string {
	for _, name := range []string{"X-GitHub-Delivery", "X-Gitea-Delivery", "X-Forgejo-Delivery", "X-Gitlab-Event-UUID", "Idempotency-Key"} {
		if v := h.Get(name); v != "" {
			return shortLine(v, 64)
		}
	}
	return ""
}

// ValidSHA reports whether s is a full lower-case hex commit SHA.
func ValidSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
