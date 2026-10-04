package git

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// GitHub REST API (docs.github.com/en/rest). github.com's API lives at
// api.github.com; GitHub Enterprise Server's at https://<host>/api/v3.
//
// Tokens (fine-grained recommended) need, on the repositories: Contents
// read (clone, branches, Dockerfile), Metadata read, Commit statuses write
// (checks on commits) and Webhooks write (automatic webhooks).
//
// A GitHub App authenticates as itself with a short JWT signed by its
// private key, exchanges it for an installation token (one hour), and
// reports check runs, which only Apps may create. Its permissions: Contents
// read, Metadata read, Checks write; events: Push and Pull request. Its one
// webhook is the App's own, which Kwerft points at the console.
type github struct {
	f    *Factory
	conn Connection
	api  *apiClient
	// app is the App's own API identity (JWT), for /app endpoints.
	app *apiClient
}

func githubAPIBase(connURL string) string {
	u, err := url.Parse(connURL)
	if err == nil && strings.EqualFold(u.Hostname(), "github.com") {
		return "https://api.github.com"
	}
	return strings.TrimSuffix(connURL, "/") + "/api/v3"
}

func newGitHub(f *Factory, c Connection) *github {
	g := &github{f: f, conn: c}
	header := map[string]string{"Accept": "application/vnd.github+json"}
	base := githubAPIBase(c.URL)
	g.api = &apiClient{http: f.httpClient(), base: base, header: header}
	switch c.Auth {
	case kwerftv1.GitAuthToken:
		g.api.auth = bearer(c.Token)
	case kwerftv1.GitAuthGitHubApp:
		g.app = &apiClient{http: f.httpClient(), base: base, header: header, auth: func(_ context.Context, req *http.Request) error {
			jwt, err := g.appJWT()
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+jwt)
			return nil
		}}
		g.api.auth = func(ctx context.Context, req *http.Request) error {
			tok, err := g.installationToken(ctx)
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			return nil
		}
	}
	return g
}

func bearer(token string) func(context.Context, *http.Request) error {
	return func(_ context.Context, req *http.Request) error {
		if token == "" {
			return fmt.Errorf("%w: no token stored", ErrUnauthorized)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	}
}

// ParseAppKey parses a GitHub App private key (the PEM file GitHub offers
// for download: PKCS#1, or PKCS#8).
func ParseAppKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("not a PEM private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("not an RSA private key")
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA private key")
	}
	return rk, nil
}

// appJWT is the App's own credential: RS256, issued a minute in the past
// against clock drift, valid for nine minutes (GitHub allows ten).
func (g *github) appJWT() (string, error) {
	key, err := ParseAppKey(g.conn.GitHubAppKey)
	if err != nil {
		return "", fmt.Errorf("%w: GitHub App private key: %v", ErrUnauthorized, err)
	}
	now := g.f.now()
	return SignJWT(key, map[string]any{
		"iat": now.Add(-time.Minute).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": strconv.FormatInt(g.conn.AppID, 10),
	})
}

// SignJWT signs claims with RS256.
func SignJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(head) + "." + enc.EncodeToString(body)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// installationToken returns a cached installation token, or fetches one.
func (g *github) installationToken(ctx context.Context) (string, error) {
	// The key's hash is part of the cache key, so a replaced key never
	// reuses the old key's token.
	sum := sha256.Sum256(g.conn.GitHubAppKey)
	cacheKey := fmt.Sprintf("%s|%d|%d|%s", g.conn.URL, g.conn.AppID, g.conn.InstallationID, hex.EncodeToString(sum[:8]))
	g.f.mu.Lock()
	if t, ok := g.f.tokens[cacheKey]; ok && g.f.now().Before(t.expires) {
		g.f.mu.Unlock()
		return t.token, nil
	}
	g.f.mu.Unlock()

	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := g.app.do(ctx, "POST", fmt.Sprintf("/app/installations/%d/access_tokens", g.conn.InstallationID), nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("GitHub returned no installation token")
	}
	expires := out.ExpiresAt.Add(-5 * time.Minute)
	if out.ExpiresAt.IsZero() {
		expires = g.f.now().Add(50 * time.Minute)
	}
	g.f.mu.Lock()
	if g.f.tokens == nil || len(g.f.tokens) > 1000 {
		g.f.tokens = map[string]installationToken{}
	}
	g.f.tokens[cacheKey] = installationToken{token: out.Token, expires: expires}
	g.f.mu.Unlock()
	return out.Token, nil
}

func (g *github) Verify(ctx context.Context) (string, error) {
	switch g.conn.Auth {
	case kwerftv1.GitAuthGitHubApp:
		var app struct {
			Slug string `json:"slug"`
		}
		if err := g.app.do(ctx, "GET", "/app", nil, &app); err != nil {
			return "", err
		}
		var inst struct {
			Account struct {
				Login string `json:"login"`
			} `json:"account"`
		}
		if err := g.app.do(ctx, "GET", fmt.Sprintf("/app/installations/%d", g.conn.InstallationID), nil, &inst); err != nil {
			return "", err
		}
		if _, err := g.installationToken(ctx); err != nil {
			return "", err
		}
		account := app.Slug + "[bot]"
		if inst.Account.Login != "" {
			account += " on " + inst.Account.Login
		}
		return account, nil
	case kwerftv1.GitAuthToken:
		var user struct {
			Login string `json:"login"`
		}
		if err := g.api.do(ctx, "GET", "/user", nil, &user); err != nil {
			return "", err
		}
		return user.Login, nil
	}
	return "", nil // public access: nothing to verify
}

func repoPath(repo Repo) string {
	return "/repos/" + escapeSegments(repo.Path)
}

// escapeSegments escapes each path segment, keeping the slashes.
func escapeSegments(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (g *github) DefaultBranch(ctx context.Context, repo Repo) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo), nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

type githubCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string    `json:"name"`
			Date time.Time `json:"date"`
		} `json:"author"`
	} `json:"commit"`
}

func (c githubCommit) commit() Commit {
	return Commit{SHA: c.SHA, Message: c.Commit.Message, Author: c.Commit.Author.Name, Time: c.Commit.Author.Date}
}

// Head uses GET /repos/{owner}/{repo}/branches/{branch}, whose commit is
// shaped like the commits endpoint's.
func (g *github) Head(ctx context.Context, repo Repo, branch string) (Commit, error) {
	var out struct {
		Commit githubCommit `json:"commit"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/branches/"+escapeSegments(branch), nil, &out); err != nil {
		return Commit{}, err
	}
	return out.Commit.commit(), nil
}

func (g *github) Commit(ctx context.Context, repo Repo, sha string) (Commit, error) {
	var out githubCommit
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/commits/"+url.PathEscape(sha), nil, &out); err != nil {
		return Commit{}, err
	}
	return out.commit(), nil
}

func (g *github) FileExists(ctx context.Context, repo Repo, ref, path string) (bool, error) {
	err := g.api.do(ctx, "GET", repoPath(repo)+"/contents/"+escapeSegments(strings.Trim(path, "/"))+"?ref="+url.QueryEscape(ref), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (g *github) hookConfig(hook Hook) map[string]any {
	return map[string]any{"url": hook.URL, "content_type": "json", "secret": hook.Secret, "insecure_ssl": "0"}
}

func (g *github) ConnectionWebhook(ctx context.Context, hook Hook) (bool, error) {
	if g.conn.Auth != kwerftv1.GitAuthGitHubApp {
		return false, nil
	}
	// The App's events (push, pull request) are chosen in its settings;
	// the API only sets where they go.
	return true, g.app.do(ctx, "PATCH", "/app/hook/config", g.hookConfig(hook), nil)
}

func (g *github) EnsureWebhook(ctx context.Context, repo Repo, hook Hook, update bool) error {
	if g.conn.Auth != kwerftv1.GitAuthToken {
		return ErrUnsupported
	}
	var hooks []struct {
		ID     int64 `json:"id"`
		Config struct {
			URL string `json:"url"`
		} `json:"config"`
	}
	if err := g.api.do(ctx, "GET", repoPath(repo)+"/hooks?per_page=100", nil, &hooks); err != nil {
		return err
	}
	body := map[string]any{"active": true, "events": []string{"push", "pull_request"}, "config": g.hookConfig(hook)}
	for _, h := range hooks {
		if h.Config.URL == hook.URL {
			if !update {
				return nil
			}
			return g.api.do(ctx, "PATCH", fmt.Sprintf("%s/hooks/%d", repoPath(repo), h.ID), body, nil)
		}
	}
	body["name"] = "web"
	return g.api.do(ctx, "POST", repoPath(repo)+"/hooks", body, nil)
}

func (g *github) ReportStatus(ctx context.Context, repo Repo, st Status) (string, error) {
	switch g.conn.Auth {
	case kwerftv1.GitAuthGitHubApp:
		return g.checkRun(ctx, repo, st)
	case kwerftv1.GitAuthToken:
		state := map[State]string{StatePending: "pending", StateRunning: "pending", StateSuccess: "success",
			StateFailure: "failure", StateCancelled: "error"}[st.State]
		body := map[string]any{"state": state, "context": st.Context, "description": shortLine(st.Description, 140)}
		if st.TargetURL != "" {
			body["target_url"] = st.TargetURL
		}
		return "", g.api.do(ctx, "POST", repoPath(repo)+"/statuses/"+url.PathEscape(st.SHA), body, nil)
	}
	return "", ErrUnsupported
}

// checkRun creates the App's check run for the build on the first report
// and updates it afterwards.
func (g *github) checkRun(ctx context.Context, repo Repo, st Status) (string, error) {
	body := map[string]any{
		"name":        st.Context,
		"external_id": st.ExternalID,
		"output":      map[string]string{"title": shortLine(st.Description, 140), "summary": st.Description},
	}
	if st.TargetURL != "" {
		body["details_url"] = st.TargetURL
	}
	switch st.State {
	case StatePending:
		body["status"] = "queued"
	case StateRunning:
		body["status"] = "in_progress"
	default:
		body["status"] = "completed"
		body["conclusion"] = map[State]string{StateSuccess: "success", StateFailure: "failure", StateCancelled: "cancelled"}[st.State]
	}
	if st.Ref != "" {
		if err := g.api.do(ctx, "PATCH", repoPath(repo)+"/check-runs/"+url.PathEscape(st.Ref), body, nil); !errors.Is(err, ErrNotFound) {
			return st.Ref, err
		}
		// The check run is gone (or never was ours): start a new one.
	}
	body["head_sha"] = st.SHA
	var out struct {
		ID int64 `json:"id"`
	}
	if err := g.api.do(ctx, "POST", repoPath(repo)+"/check-runs", body, &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}
