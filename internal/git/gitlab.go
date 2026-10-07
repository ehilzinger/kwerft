// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// GitLab REST API v4 (docs.gitlab.com/api). Projects are addressed by their
// URL-encoded path (group%2Fsub%2Fname). A personal, group or project access
// token with the "api" scope and the Maintainer role creates project hooks
// and sets commit statuses; "read_api" + "read_repository" only builds.
//
// Webhooks authenticate with the secret token GitLab sends back verbatim in
// X-Gitlab-Token. GitLab 19 also offers signing tokens (HMAC), but GitLab
// generates those itself and the hooks API cannot set them, so Kwerft uses
// the secret token; it travels over HTTPS only.
type gitlab struct {
	conn Connection
	api  *apiClient
}

func newGitLab(f *Factory, c Connection) *gitlab {
	api := &apiClient{http: f.httpClient(), base: strings.TrimSuffix(c.URL, "/") + "/api/v4"}
	if c.Auth == kwerftv1.GitAuthToken {
		token := c.Token
		api.auth = func(_ context.Context, req *http.Request) error {
			if token == "" {
				return fmt.Errorf("%w: no token stored", ErrUnauthorized)
			}
			req.Header.Set("PRIVATE-TOKEN", token)
			return nil
		}
	}
	return &gitlab{conn: c, api: api}
}

// projectPath is /projects/<url-encoded path>; slashes become %2F.
func projectPath(repo Repo) string {
	return "/projects/" + escapeAll(repo.Path)
}

func escapeAll(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "/", "%2F")
}

func (g *gitlab) Verify(ctx context.Context) (string, error) {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return "", nil
	}
	var user struct {
		Username string `json:"username"`
	}
	if err := g.api.do(ctx, "GET", "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Username, nil
}

func (g *gitlab) DefaultBranch(ctx context.Context, repo Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.api.do(ctx, "GET", projectPath(repo), nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

type gitlabCommit struct {
	ID            string    `json:"id"`
	Message       string    `json:"message"`
	AuthorName    string    `json:"author_name"`
	CommittedDate time.Time `json:"committed_date"`
}

// commit reads GET /projects/:id/repository/commits/:sha, which also takes
// a branch name.
func (g *gitlab) commit(ctx context.Context, repo Repo, ref string) (Commit, error) {
	var out gitlabCommit
	if err := g.api.do(ctx, "GET", projectPath(repo)+"/repository/commits/"+escapeAll(ref), nil, &out); err != nil {
		return Commit{}, err
	}
	return Commit{SHA: out.ID, Message: out.Message, Author: out.AuthorName, Time: out.CommittedDate}, nil
}

func (g *gitlab) Head(ctx context.Context, repo Repo, branch string) (Commit, error) {
	return g.commit(ctx, repo, branch)
}

func (g *gitlab) Commit(ctx context.Context, repo Repo, sha string) (Commit, error) {
	return g.commit(ctx, repo, sha)
}

func (g *gitlab) FileExists(ctx context.Context, repo Repo, ref, path string) (bool, error) {
	err := g.api.do(ctx, "HEAD", projectPath(repo)+"/repository/files/"+escapeAll(strings.Trim(path, "/"))+"?ref="+url.QueryEscape(ref), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (g *gitlab) ConnectionWebhook(context.Context, Hook) (bool, error) { return false, nil }

func (g *gitlab) EnsureWebhook(ctx context.Context, repo Repo, hook Hook, update bool) error {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return ErrUnsupported
	}
	var hooks []struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if err := g.api.do(ctx, "GET", projectPath(repo)+"/hooks?per_page=100", nil, &hooks); err != nil {
		return err
	}
	body := map[string]any{"url": hook.URL, "token": hook.Secret, "push_events": true, "merge_requests_events": true,
		"enable_ssl_verification": true, "name": "Kwerft"}
	for _, h := range hooks {
		if h.URL == hook.URL {
			if !update {
				return nil
			}
			return g.api.do(ctx, "PUT", fmt.Sprintf("%s/hooks/%d", projectPath(repo), h.ID), body, nil)
		}
	}
	return g.api.do(ctx, "POST", projectPath(repo)+"/hooks", body, nil)
}

func (g *gitlab) ReportStatus(ctx context.Context, repo Repo, st Status) (string, error) {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return "", ErrUnsupported
	}
	state := map[State]string{StatePending: "pending", StateRunning: "running", StateSuccess: "success",
		StateFailure: "failed", StateCancelled: "canceled"}[st.State]
	body := map[string]any{"state": state, "name": st.Context, "description": shortLine(st.Description, 255)}
	if st.TargetURL != "" && len(st.TargetURL) <= 255 {
		body["target_url"] = st.TargetURL
	}
	err := g.api.do(ctx, "POST", projectPath(repo)+"/statuses/"+url.PathEscape(st.SHA), body, nil)
	// GitLab refuses to set the state a status already has ("Cannot
	// transition status via :run from :running"): a retried report.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest && strings.Contains(apiErr.Message, "transition") {
		return "", nil
	}
	return "", err
}
