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

// Gitea and Forgejo (Codeberg) REST API v1 (/api/swagger on any instance).
// An access token with the repository scope "write" (and "read:user" for
// the account name) clones, creates hooks and sets commit statuses. Gitea
// signs webhooks with HMAC-SHA256 of the body (X-Gitea-Signature, Forgejo
// also X-Forgejo-Signature; both hex).
type gitea struct {
	conn Connection
	api  *apiClient
}

func newGitea(f *Factory, c Connection) *gitea {
	api := &apiClient{http: f.httpClient(), base: strings.TrimSuffix(c.URL, "/") + "/api/v1"}
	if c.Auth == kwerftv1.GitAuthToken {
		token := c.Token
		api.auth = func(_ context.Context, req *http.Request) error {
			if token == "" {
				return fmt.Errorf("%w: no token stored", ErrUnauthorized)
			}
			req.Header.Set("Authorization", "token "+token)
			return nil
		}
	}
	return &gitea{conn: c, api: api}
}

func (g *gitea) Verify(ctx context.Context) (string, error) {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return "", nil
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := g.api.do(ctx, "GET", "/user", nil, &user); err != nil {
		return "", err
	}
	return user.Login, nil
}

func (g *gitea) DefaultBranch(ctx context.Context, repo Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo), nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

func (g *gitea) Head(ctx context.Context, repo Repo, branch string) (Commit, error) {
	var out struct {
		Commit struct {
			ID      string `json:"id"`
			Message string `json:"message"`
			Author  struct {
				Name string `json:"name"`
			} `json:"author"`
			Timestamp time.Time `json:"timestamp"`
		} `json:"commit"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/branches/"+escapeSegments(branch), nil, &out); err != nil {
		return Commit{}, err
	}
	c := out.Commit
	return Commit{SHA: c.ID, Message: c.Message, Author: c.Author.Name, Time: c.Timestamp}, nil
}

// Commit reads GET /repos/{owner}/{repo}/git/commits/{sha}, shaped like
// GitHub's.
func (g *gitea) Commit(ctx context.Context, repo Repo, sha string) (Commit, error) {
	var out githubCommit
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/git/commits/"+url.PathEscape(sha), nil, &out); err != nil {
		return Commit{}, err
	}
	return out.commit(), nil
}

func (g *gitea) FileExists(ctx context.Context, repo Repo, ref, path string) (bool, error) {
	err := g.api.do(ctx, "GET", repoPath(repo)+"/contents/"+escapeSegments(strings.Trim(path, "/"))+"?ref="+url.QueryEscape(ref), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (g *gitea) ConnectionWebhook(context.Context, Hook) (bool, error) { return false, nil }

func (g *gitea) EnsureWebhook(ctx context.Context, repo Repo, hook Hook, update bool) error {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return ErrUnsupported
	}
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/hooks?limit=50", nil, &hooks); err != nil {
		return err
	}
	// "pull_request" includes synchronize events (new commits on a PR).
	body := map[string]any{"active": true, "events": []string{"push", "pull_request"},
		"config": map[string]string{"url": hook.URL, "content_type": "json", "secret": hook.Secret}}
	for _, h := range hooks {
		if h.Config.URL == hook.URL {
			if !update {
				return nil
			}
			return g.api.do(ctx, "PATCH", fmt.Sprintf("%s/hooks/%d", repoPath(repo), h.ID), body, nil)
		}
	}
	body["type"] = "gitea"
	return g.api.do(ctx, "POST", repoPath(repo)+"/hooks", body, nil)
}

func (g *gitea) ReportStatus(ctx context.Context, repo Repo, st Status) (string, error) {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return "", ErrUnsupported
	}
	state := map[State]string{StatePending: "pending", StateRunning: "pending", StateSuccess: "success",
		StateFailure: "failure", StateCancelled: "error"}[st.State]
	body := map[string]any{"state": state, "context": st.Context, "description": shortLine(st.Description, 140)}
	if st.TargetURL != "" {
		body["target_url"] = st.TargetURL
	}
	return "", g.api.do(ctx, "POST", repoPath(repo)+"/statuses/"+url.PathEscape(st.SHA), body, nil)
}
