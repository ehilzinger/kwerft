// Package git talks to Git hosts on behalf of a GitConnection: it checks
// credentials, resolves branch heads, finds Dockerfiles, creates the webhooks
// that trigger builds and reports build results back as commit statuses or
// check runs. GitHub (token or GitHub App), GitLab and Gitea/Forgejo are
// spoken to through their REST APIs; any other host ("generic"), and every
// SSH deploy key, through the Git protocol itself (ls-remote), which knows
// branch heads but nothing else.
//
// Credentials only ever travel in request headers to the connection's own
// host. Errors name the request (method and path), never a header, so they
// are safe to log and to show in a status condition.
package git

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

var (
	// ErrUnauthorized: the host rejected the credentials (HTTP 401).
	ErrUnauthorized = errors.New("the Git host rejected the credentials")
	// ErrForbidden: the credentials are valid but lack a permission (403).
	ErrForbidden = errors.New("the credentials lack the permission for this")
	// ErrNotFound: no such repository, branch or file, or no access to it
	// (hosts answer 404 for private repositories the caller cannot see).
	ErrNotFound = errors.New("not found, or the credentials have no access to it")
	// ErrUnsupported: the host or the kind of credentials cannot do this;
	// e.g. a deploy key cannot create webhooks or report commit statuses.
	ErrUnsupported = errors.New("not supported with this kind of connection")
)

// Commit is what Kwerft shows about a commit.
type Commit struct {
	SHA     string    `json:"sha"`
	Message string    `json:"message"`
	Author  string    `json:"author"`
	Time    time.Time `json:"time,omitzero"`
}

// Connection is a GitConnection with its credentials.
type Connection struct {
	Name     string
	Provider kwerftv1.GitProvider
	URL      string // https://host[/prefix]
	Auth     kwerftv1.GitAuth
	Owner    string

	AppID, InstallationID int64

	Token         string
	SSHPrivateKey []byte
	KnownHosts    []byte
	GitHubAppKey  []byte
}

// Credential Secret keys; the same as internal/builds (which this package
// does not import, to stay a leaf).
const (
	keyToken         = "token"
	keySSHPrivateKey = "ssh-privatekey"
	keyKnownHosts    = "known_hosts"
	keyGitHubAppKey  = "github-app-private-key"
)

// ConnectionFor combines a GitConnection with its credentials Secret's data.
func ConnectionFor(gc *kwerftv1.GitConnection, secret map[string][]byte) Connection {
	c := Connection{
		Name: gc.Name, Provider: gc.Spec.Provider, URL: strings.TrimSuffix(gc.Spec.URL, "/"), Auth: gc.Spec.Auth,
		Owner: gc.Spec.Owner,
	}
	if gc.Spec.GitHubApp != nil {
		c.AppID, c.InstallationID = gc.Spec.GitHubApp.AppID, gc.Spec.GitHubApp.InstallationID
	}
	if secret != nil {
		c.Token = strings.TrimSpace(string(secret[keyToken]))
		c.SSHPrivateKey = secret[keySSHPrivateKey]
		c.KnownHosts = secret[keyKnownHosts]
		c.GitHubAppKey = secret[keyGitHubAppKey]
	}
	return c
}

// Host is the connection URL's host name.
func (c Connection) Host() string {
	u, err := url.Parse(c.URL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Covers explains why the connection cannot be used for repo, or returns nil.
func (c Connection) Covers(repo Repo) error {
	if host := c.Host(); host != repo.Host {
		return fmt.Errorf("the connection is for %s, not %s", host, repo.Host)
	}
	if c.Owner != "" && !strings.EqualFold(c.Owner, repo.Owner()) {
		return fmt.Errorf("the connection is limited to %s, not %s", c.Owner, repo.Owner())
	}
	return nil
}

// ReportsStatus says whether builds report commit statuses with this
// connection: tokens and GitHub Apps on a known provider.
func (c Connection) ReportsStatus() bool {
	return c.Provider != kwerftv1.Generic && (c.Auth == kwerftv1.GitAuthToken || c.Auth == kwerftv1.GitAuthGitHubApp)
}

// usesAPI says whether reads go through the provider's REST API rather than
// the Git protocol. A deploy key opens only the Git protocol (over SSH).
func (c Connection) usesAPI() bool {
	return c.Provider != kwerftv1.Generic && c.Auth != kwerftv1.GitAuthSSHKey
}

// Public is a credential-less connection for a repository that is not
// covered by any GitConnection: public repositories over HTTPS. The
// provider is guessed from the host.
func Public(repo Repo) Connection {
	p := kwerftv1.Generic
	switch repo.Host {
	case "github.com":
		p = kwerftv1.GitHub
	case "gitlab.com":
		p = kwerftv1.GitLab
	case "codeberg.org", "gitea.com":
		p = kwerftv1.Gitea
	}
	return Connection{Provider: p, URL: "https://" + repo.Host, Auth: kwerftv1.GitAuthNone}
}

// State is a build's result as Git hosts show it.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSuccess   State = "success"
	StateFailure   State = "failure"
	StateCancelled State = "cancelled"
)

// Status is one report on a commit.
type Status struct {
	SHA         string
	State       State
	Context     string // e.g. kwerft/shop/api; one status per App
	Description string // one line, at most 140 characters
	TargetURL   string // the build in the console
	ExternalID  string // the Build's namespace/name (GitHub check runs)
	// Ref is what an earlier report returned (a GitHub check run ID), so
	// later phases update the same check instead of adding one.
	Ref string
}

// Hook is the webhook Kwerft wants on a repository (or GitHub App).
type Hook struct {
	URL    string
	Secret string
}

// Provider is one Git host as seen through one connection's credentials.
type Provider interface {
	// Verify checks the credentials and names the account they act as.
	Verify(ctx context.Context) (account string, err error)
	// DefaultBranch is the repository's default branch.
	DefaultBranch(ctx context.Context, repo Repo) (string, error)
	// Head resolves a branch to its newest commit.
	Head(ctx context.Context, repo Repo, branch string) (Commit, error)
	// Commit describes a commit by its full SHA.
	Commit(ctx context.Context, repo Repo, sha string) (Commit, error)
	// FileExists checks for a file (e.g. the Dockerfile) at ref.
	FileExists(ctx context.Context, repo Repo, ref, path string) (bool, error)
	// ConnectionWebhook sets a webhook that covers every repository the
	// credentials reach (a GitHub App's own webhook). handled is false when
	// the connection has none, and webhooks go per repository instead.
	ConnectionWebhook(ctx context.Context, hook Hook) (handled bool, err error)
	// EnsureWebhook creates the repository webhook if it is missing; update
	// also rewrites an existing one (after the secret changed).
	EnsureWebhook(ctx context.Context, repo Repo, hook Hook, update bool) error
	// ReportStatus reports a build result on a commit and returns a
	// reference for updating the same report later (see Status.Ref).
	ReportStatus(ctx context.Context, repo Repo, st Status) (ref string, err error)
}

// Factory builds Providers. It holds the HTTP client and caches GitHub App
// installation tokens (valid for an hour) across calls.
type Factory struct {
	// HTTP is the client for every API and Git request; nil means one with
	// a 20-second timeout. Tests pass the fake's TLS client.
	HTTP *http.Client
	// Now is the clock for App tokens; nil means time.Now.
	Now func() time.Time
	// SSHTimeout bounds ls-remote over SSH; zero means 20 seconds.
	SSHTimeout time.Duration

	mu     sync.Mutex
	tokens map[string]installationToken
}

type installationToken struct {
	token   string
	expires time.Time
}

func (f *Factory) httpClient() *http.Client {
	if f.HTTP != nil {
		return f.HTTP
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (f *Factory) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// For returns the Provider for a connection.
func (f *Factory) For(c Connection) (Provider, error) {
	c.URL = strings.TrimSuffix(c.URL, "/")
	if _, err := url.Parse(c.URL); err != nil || !strings.HasPrefix(c.URL, "https://") {
		return nil, fmt.Errorf("connection URL %q is not an https URL", c.URL)
	}
	if !c.usesAPI() {
		return &protocolProvider{f: f, conn: c}, nil
	}
	switch c.Provider {
	case kwerftv1.GitHub:
		return newGitHub(f, c), nil
	case kwerftv1.GitLab:
		return newGitLab(f, c), nil
	case kwerftv1.Gitea:
		return newGitea(f, c), nil
	}
	return nil, fmt.Errorf("unknown provider %q", c.Provider)
}

// shortLine is the first line of a message, at most n bytes.
func shortLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > n {
		s = strings.ToValidUTF8(s[:n], "") // drop a cut multi-byte character
	}
	return s
}
