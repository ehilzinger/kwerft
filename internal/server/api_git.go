// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/git"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Git connections, repository checks and "Build now" (docs/phase2.md).
//
// Connections are cluster-scoped GitConnections. Everyone signed in reads
// them (the deploy wizard offers them); owners and admins create, change
// and delete them — the console checks the role, and Kubernetes does again
// on the impersonated write (kwerft.dev "*" is theirs alone).
//
// Credentials are write-only, like the DNS token in api_settings.go. They
// live in the Secret git-<name> in kwerft-builds, which the GitConnection
// reconciler creates and lists by name in the Role kwerft:git-credentials
// (verb patch, owners and admins). The console writes credentials with a
// merge patch as the signed-in user, so Kubernetes decides; nobody can get
// or list those Secrets through the console, no endpoint returns them, and
// the audit log only records that they changed. The one exception is the
// webhook secret, which the console generates and shows exactly once (on
// create and on rotation), since it has to be pasted into the Git host
// when the webhook cannot be created automatically.
//
// The console reads credentials only with its own identity (SystemReader)
// and only to talk to the Git host on a user's behalf after Kubernetes let
// that user read the connection (repository checks, resolving the branch
// head for "Build now"), and for webhooks (verifying signatures).
//
// "Build now" creates the Build as the user (impersonated); webhook builds
// are created with the console's own identity, since no user initiated
// them, and are audited as git:<connection>.

type gitAPI struct {
	*api
	git *git.Factory
	// hookIP bounds webhook deliveries per client address; hookFail bounds
	// how many rejected deliveries per address reach the audit log.
	hookIP   *limiter
	hookFail *limiter
	// credentialsWait is how long a write waits for the reconciler to
	// create a new connection's Secret and grant access to it.
	credentialsWait time.Duration
	maxHookBody     int64
}

func (a *api) registerGit(mux *http.ServeMux) {
	g := &gitAPI{
		api: a, git: a.cfg.Git,
		hookIP:          newLimiter(120, time.Minute, a.now),
		hookFail:        newLimiter(10, 15*time.Minute, a.now),
		credentialsWait: 15 * time.Second,
		maxHookBody:     8 << 20,
	}
	if g.git == nil {
		g.git = &git.Factory{}
	}
	if a.cfg.gitHook != nil {
		a.cfg.gitHook(g)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return write(a.requireRole(h, store.RoleOwner, store.RoleAdmin))
	}
	mux.HandleFunc("GET /api/v1/git/connections", read(g.list))
	mux.HandleFunc("POST /api/v1/git/connections", admin(g.create))
	mux.HandleFunc("PUT /api/v1/git/connections/{name}", admin(g.update))
	mux.HandleFunc("DELETE /api/v1/git/connections/{name}", admin(g.delete))
	mux.HandleFunc("POST /api/v1/git/connections/{name}/webhook-secret", admin(g.rotateWebhookSecret))
	// The deploy wizard: whoever may deploy (viewers may not).
	mux.HandleFunc("POST /api/v1/git/check", write(a.requireRole(g.check, store.RoleOwner, store.RoleAdmin, store.RoleDeveloper)))
	// Kubernetes decides (create builds in the project).
	mux.HandleFunc("POST /api/v1/projects/{project}/apps/{app}/builds", write(g.buildNow))
	// Git hosts: no session, no same-origin check; the signature is the
	// authentication (api_git_hooks.go).
	mux.HandleFunc("POST /api/v1/hooks/git/{connection}", g.hook)
}

// ---- views -------------------------------------------------------------------------

type gitHubAppJSON struct {
	AppID          int64  `json:"appID"`
	InstallationID int64  `json:"installationID"`
	Slug           string `json:"slug,omitempty"`
}

type webhookJSON struct {
	Repository string `json:"repository"`
	Automatic  bool   `json:"automatic"`
	Message    string `json:"message,omitempty"`
}

type connectionJSON struct {
	Name             string         `json:"name"`
	Provider         string         `json:"provider"`
	URL              string         `json:"url"`
	Auth             string         `json:"auth"`
	Owner            string         `json:"owner,omitempty"`
	Projects         []string       `json:"projects"`
	Account          string         `json:"account,omitempty"`
	Ready            bool           `json:"ready"`
	Message          string         `json:"message,omitempty"`
	WebhookURL       string         `json:"webhookURL,omitempty"`
	WebhookAutomatic bool           `json:"webhookAutomatic"`
	Webhooks         []webhookJSON  `json:"webhooks"`
	LastDelivery     *time.Time     `json:"lastDelivery,omitempty"`
	GitHubApp        *gitHubAppJSON `json:"githubApp,omitempty"`
}

func connectionView(gc *kwerftv1.GitConnection) connectionJSON {
	out := connectionJSON{
		Name: gc.Name, Provider: string(gc.Spec.Provider), URL: gc.Spec.URL, Auth: string(gc.Spec.Auth), Owner: gc.Spec.Owner,
		Projects: gc.Spec.Projects, Account: gc.Status.Account, WebhookURL: gc.Status.WebhookURL,
		WebhookAutomatic: gc.Status.WebhookAutomatic, LastDelivery: timePtr(gc.Status.LastDelivery), Webhooks: []webhookJSON{},
	}
	if out.Projects == nil {
		out.Projects = []string{}
	}
	if app := gc.Spec.GitHubApp; app != nil {
		out.GitHubApp = &gitHubAppJSON{AppID: app.AppID, InstallationID: app.InstallationID, Slug: app.Slug}
	}
	for _, h := range gc.Status.Webhooks {
		out.Webhooks = append(out.Webhooks, webhookJSON{Repository: h.Repository, Automatic: h.Automatic, Message: h.Message})
	}
	if c := meta.FindStatusCondition(gc.Status.Conditions, controllers.ConditionReady); c != nil {
		out.Message = c.Message
		out.Ready = c.Status == metav1.ConditionTrue && c.ObservedGeneration == gc.Generation
	} else {
		out.Message = "Checking the connection…"
	}
	return out
}

func connectionNotFound(name string) string { return fmt.Sprintf("Git connection %q not found.", name) }

func (g *gitAPI) list(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	// Every role may list kwerft.dev resources (roles.yaml), and the list
	// carries no credentials, so the cache may serve it (see
	// api_workloads.go for the rule).
	var list kwerftv1.GitConnectionList
	if err := g.api.list(ctx, c, &list); err != nil {
		g.kubeError(w, r, p, "git.connection_list", "", "Git connections not found.", err)
		return
	}
	out := make([]connectionJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, connectionView(&list.Items[i]))
	}
	slices.SortFunc(out, func(a, b connectionJSON) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

// ---- input -------------------------------------------------------------------------

type connectionInput struct {
	Name          string   `json:"name"`
	Provider      string   `json:"provider"`
	URL           string   `json:"url"`
	Auth          string   `json:"auth"`
	Owner         string   `json:"owner"`
	Projects      []string `json:"projects"`
	Token         string   `json:"token"`
	SSHPrivateKey string   `json:"sshPrivateKey"`
	KnownHosts    string   `json:"knownHosts"`
	GitHubApp     *struct {
		AppID          int64  `json:"appID"`
		InstallationID int64  `json:"installationID"`
		PrivateKey     string `json:"privateKey"`
	} `json:"githubApp"`
}

// credentialKeys are the Secret keys each kind of auth uses.
var credentialKeys = map[kwerftv1.GitAuth][]string{
	kwerftv1.GitAuthNone:      nil,
	kwerftv1.GitAuthToken:     {builds.KeyToken},
	kwerftv1.GitAuthSSHKey:    {builds.KeySSHPrivateKey, builds.KeyKnownHosts},
	kwerftv1.GitAuthGitHubApp: {builds.KeyGitHubAppKey},
}

var allCredentialKeys = []string{builds.KeyToken, builds.KeySSHPrivateKey, builds.KeyKnownHosts, builds.KeyGitHubAppKey}

// parsedInput is a validated ConnectionInput.
type parsedInput struct {
	spec  kwerftv1.GitConnectionSpec
	creds map[string]string // Secret key → value, only what was entered
}

// credentialField is the input field holding a Secret key, for errors.
func credentialField(key string) string {
	switch key {
	case builds.KeyToken:
		return "token"
	case builds.KeySSHPrivateKey:
		return "sshPrivateKey"
	case builds.KeyKnownHosts:
		return "knownHosts"
	case builds.KeyGitHubAppKey:
		return "githubApp.privateKey"
	}
	return key
}

// validateInput checks a ConnectionInput; needCreds says the credentials
// for its auth must be entered (create, or a changed host or auth).
func validateInput(w http.ResponseWriter, in *connectionInput, needCreds func(kwerftv1.GitConnectionSpec) bool) (*parsedInput, bool) {
	out := &parsedInput{creds: map[string]string{}}
	spec := &out.spec
	spec.Provider = kwerftv1.GitProvider(strings.TrimSpace(in.Provider))
	switch spec.Provider {
	case kwerftv1.GitHub, kwerftv1.GitLab, kwerftv1.Gitea, kwerftv1.Generic:
	default:
		writeFieldError(w, "provider", "Choose GitHub, GitLab, Gitea/Forgejo or another Git host.")
		return nil, false
	}
	raw := strings.TrimSuffix(strings.TrimSpace(in.URL), "/")
	if raw == "" && spec.Provider == kwerftv1.GitHub {
		raw = "https://github.com"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, " \t") {
		writeFieldError(w, "url", "Enter the host's address, like https://gitlab.example.com.")
		return nil, false
	}
	spec.URL = "https://" + strings.ToLower(u.Host) + u.EscapedPath()
	spec.Auth = kwerftv1.GitAuth(strings.TrimSpace(in.Auth))
	if _, ok := credentialKeys[spec.Auth]; !ok {
		writeFieldError(w, "auth", "Choose how Kwerft signs in: none, an access token, an SSH deploy key or a GitHub App.")
		return nil, false
	}
	if spec.Auth == kwerftv1.GitAuthGitHubApp && spec.Provider != kwerftv1.GitHub {
		writeFieldError(w, "auth", "GitHub Apps only exist on GitHub.")
		return nil, false
	}
	spec.Owner = strings.TrimSpace(in.Owner)
	if len(spec.Owner) > 100 || strings.ContainsAny(spec.Owner, " \t/\\") {
		writeFieldError(w, "owner", "Enter one account, organisation or group name, like acme.")
		return nil, false
	}
	for _, p := range in.Projects {
		p = strings.TrimSpace(p)
		if errs := validation.IsDNS1123Label(p); len(errs) > 0 {
			writeFieldError(w, "projects", fmt.Sprintf("%q is not a project name.", p))
			return nil, false
		}
		if !slices.Contains(spec.Projects, p) {
			spec.Projects = append(spec.Projects, p)
		}
	}
	slices.Sort(spec.Projects)

	if token := strings.TrimSpace(in.Token); token != "" {
		if len(token) > 1024 || strings.ContainsAny(token, " \t\r\n") {
			writeFieldError(w, "token", "That does not look like an access token.")
			return nil, false
		}
		out.creds[builds.KeyToken] = token
	}
	if key := strings.TrimSpace(in.SSHPrivateKey); key != "" {
		if _, err := ssh.ParsePrivateKey([]byte(key + "\n")); err != nil {
			msg := "That is not an SSH private key (paste the whole file, from -----BEGIN to END-----)."
			var pp *ssh.PassphraseMissingError
			if errors.As(err, &pp) {
				msg = "The key is protected by a passphrase. Deploy keys need one without."
			}
			writeFieldError(w, "sshPrivateKey", msg)
			return nil, false
		}
		out.creds[builds.KeySSHPrivateKey] = key + "\n"
	}
	if kh := strings.TrimSpace(in.KnownHosts); kh != "" {
		if _, err := git.HostKeyCallback([]byte(kh)); err != nil || len(kh) > 64<<10 {
			writeFieldError(w, "knownHosts", "Paste lines from known_hosts or ssh-keyscan, like: github.com ssh-ed25519 AAAA…")
			return nil, false
		}
		out.creds[builds.KeyKnownHosts] = kh + "\n"
	}
	if app := in.GitHubApp; app != nil {
		if spec.Auth != kwerftv1.GitAuthGitHubApp {
			writeFieldError(w, "githubApp", "GitHub App details need the GitHub App sign-in.")
			return nil, false
		}
		if app.AppID <= 0 {
			writeFieldError(w, "githubApp.appID", "Enter the App ID from the App's settings page.")
			return nil, false
		}
		if app.InstallationID <= 0 {
			writeFieldError(w, "githubApp.installationID", "Enter the installation ID (the number at the end of the installation's settings URL).")
			return nil, false
		}
		spec.GitHubApp = &kwerftv1.GitHubAppSettings{AppID: app.AppID, InstallationID: app.InstallationID}
		if key := strings.TrimSpace(app.PrivateKey); key != "" {
			if _, err := git.ParseAppKey([]byte(key)); err != nil {
				writeFieldError(w, "githubApp.privateKey", "Paste the App's private key file (.pem) as GitHub generated it.")
				return nil, false
			}
			out.creds[builds.KeyGitHubAppKey] = key + "\n"
		}
	} else if spec.Auth == kwerftv1.GitAuthGitHubApp {
		writeFieldError(w, "githubApp.appID", "Enter the App ID from the App's settings page.")
		return nil, false
	}
	// Only the credentials of the chosen auth are stored.
	for k := range out.creds {
		if !slices.Contains(credentialKeys[spec.Auth], k) {
			writeFieldError(w, credentialField(k), "This connection signs in with "+string(spec.Auth)+"; leave this empty.")
			return nil, false
		}
	}
	if needCreds(*spec) {
		for _, k := range credentialKeys[spec.Auth] {
			if k == builds.KeyKnownHosts {
				continue // optional: recorded on first use
			}
			if _, ok := out.creds[k]; !ok {
				writeFieldError(w, credentialField(k), "Enter the "+map[string]string{
					builds.KeyToken: "access token", builds.KeySSHPrivateKey: "SSH private key", builds.KeyGitHubAppKey: "App's private key",
				}[k]+".")
				return nil, false
			}
		}
	}
	return out, true
}

// verifyCredentials tries new credentials before anything is stored, so a
// typo shows up next to the field. A host that cannot be reached is only a
// warning; the reconciler keeps trying.
func (g *gitAPI) verifyCredentials(ctx context.Context, w http.ResponseWriter, name string, in *parsedInput) (warning string, ok bool) {
	if len(in.creds) == 0 {
		return "", true
	}
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: in.spec}
	data := map[string][]byte{}
	for k, v := range in.creds {
		data[k] = []byte(v)
	}
	p, err := g.git.For(git.ConnectionFor(gc, data))
	if err != nil {
		writeFieldError(w, "url", err.Error())
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, err = p.Verify(ctx)
	switch {
	case err == nil:
		return "", true
	case errors.Is(err, git.ErrUnauthorized), errors.Is(err, git.ErrForbidden), errors.Is(err, git.ErrNotFound):
		field := credentialField(credentialKeys[in.spec.Auth][0])
		writeFieldError(w, field, "The Git host rejected these credentials ("+err.Error()+").")
		return "", false
	}
	return "Could not reach the Git host to check the credentials (" + err.Error() + "). They are saved anyway.", true
}

// ---- writing credentials -----------------------------------------------------------

// writeCredentials merge-patches the connection's Secret as the user. null
// removes a key. A new connection's Secret appears, and the user's access to
// it, a moment after the connection (the reconciler creates both), so
// NotFound and Forbidden are retried for a while.
func (g *gitAPI) writeCredentials(ctx context.Context, c client.Client, name string, data map[string]*string) error {
	enc := map[string]any{}
	for k, v := range data {
		if v == nil {
			enc[k] = nil
		} else {
			enc[k] = base64.StdEncoding.EncodeToString([]byte(*v))
		}
	}
	raw, err := json.Marshal(map[string]any{"data": enc})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(g.credentialsWait)
	for {
		// A fresh object each time: the API server answers a patch with the
		// whole Secret, which must not linger anywhere.
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)}}
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		sec.Data = nil
		if err == nil || !(apierrors.IsNotFound(err) || apierrors.IsForbidden(err)) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// touchCredentials tells the reconciler that credentials changed (it does
// not watch Secrets).
func (g *gitAPI) touchCredentials(ctx context.Context, c client.Client, name string) error {
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		controllers.AnnotationCredentialsUpdated: g.now().UTC().Format(time.RFC3339Nano)}}})
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}
	return c.Patch(ctx, gc, client.RawPatch(types.MergePatchType, raw))
}

func strPtr(s string) *string { return &s }

func credentialsDetail(spec kwerftv1.GitConnectionSpec, changed map[string]*string) string {
	detail := string(spec.Provider) + " " + spec.URL + ", " + string(spec.Auth)
	var keys []string
	for k, v := range changed {
		if v != nil && k != builds.KeyWebhookSecret {
			keys = append(keys, credentialField(k))
		}
	}
	if len(keys) > 0 {
		slices.Sort(keys)
		detail += "; replaced " + strings.Join(keys, ", ")
	}
	return detail
}

// ---- create, update, delete -----------------------------------------------------------

func (g *gitAPI) create(w http.ResponseWriter, r *http.Request) {
	var in connectionInput
	if !decodeStrict(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 || len(name) > 63-len(builds.SecretPrefix) {
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 59).")
		return
	}
	parsed, ok := validateInput(w, &in, func(kwerftv1.GitConnectionSpec) bool { return true })
	if !ok {
		return
	}
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	warning, ok := g.verifyCredentials(ctx, w, name, parsed)
	if !ok {
		return
	}
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: parsed.spec}
	if err := c.Create(ctx, gc); err != nil {
		g.kubeError(w, r, p, "git.connection_create", name, connectionNotFound(name), err)
		return
	}
	g.audit(r, p.user.Email, "git.connection_create", name, credentialsDetail(parsed.spec, nil))

	webhookSecret := controllers.NewWebhookSecret()
	data := map[string]*string{builds.KeyWebhookSecret: strPtr(webhookSecret)}
	for k, v := range parsed.creds {
		data[k] = strPtr(v)
	}
	if err := g.writeCredentials(ctx, c, name, data); err != nil {
		g.cfg.Logger.Error("storing Git credentials failed", "connection", name, "err", err)
		if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
			writeError(w, http.StatusServiceUnavailable, "The connection was created, but its credentials could not be stored yet. "+
				"Edit the connection and enter them again in a moment.")
			return
		}
		g.kubeError(w, r, p, "git.credentials", name, connectionNotFound(name), err)
		return
	}
	g.audit(r, p.user.Email, "git.credentials", name, credentialsDetail(parsed.spec, data)+"; new webhook secret")
	if err := g.touchCredentials(ctx, c, name); err != nil {
		g.cfg.Logger.Error("could not mark Git credentials as updated", "connection", name, "err", err)
	}
	_ = c.Get(ctx, client.ObjectKey{Name: name}, gc)
	out := map[string]any{"connection": connectionView(gc), "webhookSecret": webhookSecret}
	if warning != "" {
		out["warning"] = warning
	}
	writeJSON(w, http.StatusCreated, out)
}

func (g *gitAPI) update(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in connectionInput
	if !decodeStrict(w, r, &in) {
		return
	}
	if in.Name != "" && in.Name != name {
		writeFieldError(w, "name", "A connection cannot be renamed. Create a new one instead.")
		return
	}
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	var gc kwerftv1.GitConnection
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &gc); err != nil {
		g.kubeError(w, r, p, "git.connection_update", name, connectionNotFound(name), err)
		return
	}
	old := gc.Spec
	// New credentials are needed when they would otherwise go to another
	// host, or when the kind of sign-in changes. A GitHub App keeps its key
	// when only its IDs change (the key belongs to the App).
	moved := func(spec kwerftv1.GitConnectionSpec) bool {
		return !strings.EqualFold(hostOf(spec.URL), hostOf(old.URL)) || spec.Auth != old.Auth ||
			spec.Auth == kwerftv1.GitAuthGitHubApp && old.GitHubApp != nil && spec.GitHubApp != nil && spec.GitHubApp.AppID != old.GitHubApp.AppID
	}
	parsed, ok := validateInput(w, &in, moved)
	if !ok {
		return
	}
	warning, ok := g.verifyCredentials(ctx, w, name, parsed)
	if !ok {
		return
	}
	if parsed.spec.GitHubApp != nil && old.GitHubApp != nil && parsed.spec.GitHubApp.AppID == old.GitHubApp.AppID {
		parsed.spec.GitHubApp.Slug = old.GitHubApp.Slug
	}
	if err := updateSpec(ctx, c, &gc, func(gc *kwerftv1.GitConnection) bool {
		if !equality.Semantic.DeepEqual(gc.Spec, old) {
			return false
		}
		gc.Spec = parsed.spec
		return true
	}); err != nil {
		g.kubeError(w, r, p, "git.connection_update", name, connectionNotFound(name), err)
		return
	}
	g.audit(r, p.user.Email, "git.connection_update", name, credentialsDetail(parsed.spec, nil))

	// Credentials: what was entered, and the removal of what no longer
	// applies (another auth, or another host).
	data := map[string]*string{}
	for k, v := range parsed.creds {
		data[k] = strPtr(v)
	}
	keep := credentialKeys[parsed.spec.Auth]
	for _, k := range allCredentialKeys {
		if _, entered := data[k]; !entered && (!slices.Contains(keep, k) || moved(parsed.spec)) {
			data[k] = nil
		}
	}
	if len(parsed.creds) > 0 || moved(parsed.spec) {
		if err := g.writeCredentials(ctx, c, name, data); err != nil {
			g.kubeError(w, r, p, "git.credentials", name, "The connection's credential storage is not ready yet. Try again in a moment.", err)
			return
		}
		g.audit(r, p.user.Email, "git.credentials", name, credentialsDetail(parsed.spec, data))
		if err := g.touchCredentials(ctx, c, name); err != nil {
			g.cfg.Logger.Error("could not mark Git credentials as updated", "connection", name, "err", err)
		}
		_ = c.Get(ctx, client.ObjectKey{Name: name}, &gc)
	}
	view := connectionView(&gc)
	if warning != "" {
		writeJSON(w, http.StatusOK, struct {
			connectionJSON
			Warning string `json:"warning"`
		}{view, warning})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}

func (g *gitAPI) delete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	// Straight from the API server, not the cache: an App created a moment
	// ago must still stop the delete.
	var apps kwerftv1.AppList
	if err := c.List(ctx, &apps); err != nil {
		g.kubeError(w, r, p, "git.connection_delete", name, "Apps not found.", err)
		return
	}
	var users []string
	for _, app := range apps.Items {
		if src := app.Spec.Source.Git; src != nil && src.Connection == name {
			users = append(users, app.Namespace+"/"+app.Name)
		}
	}
	if len(users) > 0 {
		slices.Sort(users)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "Apps still build with this connection: " + strings.Join(users, ", ") + ". Point them at another connection first.",
			"apps":  users,
		})
		return
	}
	// The reconciler (and garbage collection) remove the credentials Secret.
	if err := c.Delete(ctx, &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		g.kubeError(w, r, p, "git.connection_delete", name, connectionNotFound(name), err)
		return
	}
	g.audit(r, p.user.Email, "git.connection_delete", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// rotateWebhookSecret replaces the webhook secret and shows the new one
// once. Automatic webhooks follow at once (the reconciler pushes it to the
// host); hand-made ones need the new secret pasted.
func (g *gitAPI) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &kwerftv1.GitConnection{}); err != nil {
		g.kubeError(w, r, p, "git.webhook_secret", name, connectionNotFound(name), err)
		return
	}
	secret := controllers.NewWebhookSecret()
	if err := g.writeCredentials(ctx, c, name, map[string]*string{builds.KeyWebhookSecret: &secret}); err != nil {
		g.kubeError(w, r, p, "git.webhook_secret", name, "The connection's credential storage is not ready yet. Try again in a moment.", err)
		return
	}
	g.audit(r, p.user.Email, "git.webhook_secret", name, "rotated")
	if err := g.touchCredentials(ctx, c, name); err != nil {
		g.cfg.Logger.Error("could not mark Git credentials as updated", "connection", name, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"webhookSecret": secret})
}

// ---- talking to the Git host on a user's behalf -------------------------------------------

// errNoSystem: the console runs without its own cluster identity.
var errNoSystem = errors.New("the console has no cluster identity of its own")

// connectionFor loads a connection's credentials with the console's own
// identity. The caller has already read gc as the user.
func (g *gitAPI) connectionFor(ctx context.Context, gc *kwerftv1.GitConnection) (git.Connection, error) {
	if g.cfg.SystemReader == nil {
		return git.Connection{}, errNoSystem
	}
	var sec corev1.Secret
	err := g.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(gc.Name)}, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return git.Connection{}, err
	}
	return git.ConnectionFor(gc, sec.Data), nil
}

// hostMessage explains a Git host error for the deploy wizard.
func hostMessage(err error, repo git.Repo, conn git.Connection, what string) string {
	switch {
	case conn.Auth == kwerftv1.GitAuthNone && (errors.Is(err, git.ErrNotFound) || errors.Is(err, git.ErrUnauthorized) || errors.Is(err, git.ErrForbidden)):
		return what + " not found. If the repository is private, add a Git connection for " + repo.Host + " first."
	case errors.Is(err, git.ErrNotFound):
		return what + " not found, or the connection " + conn.Name + " has no access to it."
	case errors.Is(err, git.ErrUnauthorized):
		return repo.Host + " rejected the credentials of the connection " + conn.Name + ". Update them under Settings → Git."
	case errors.Is(err, git.ErrForbidden):
		return "The connection " + conn.Name + " may not read " + repo.String() + "."
	}
	return "Could not reach " + repo.Host + ": " + err.Error()
}

type checkResult struct {
	OK            bool        `json:"ok"`
	Message       string      `json:"message"`
	DefaultBranch string      `json:"defaultBranch,omitempty"`
	Head          *git.Commit `json:"head,omitempty"`
	Dockerfile    *bool       `json:"dockerfile,omitempty"`
	Connection    string      `json:"connection,omitempty"`
}

// check is the deploy wizard's repository check: can Kwerft see it, what
// is the branch head, is there a Dockerfile. Without a connection it picks
// a ready one that covers the repository (and the project, if given), else
// tries anonymously.
func (g *gitAPI) check(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		Connection string `json:"connection"`
		Path       string `json:"path"`
		Dockerfile string `json:"dockerfile"`
		Project    string `json:"project"`
	}
	if !decode(w, r, &req) {
		return
	}
	repo, err := git.ParseRepository(req.Repository)
	if err != nil {
		writeJSON(w, http.StatusOK, checkResult{Message: strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."})
		return
	}
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	conn := git.Public(repo)
	if req.Connection != "" {
		var gc kwerftv1.GitConnection
		if err := c.Get(ctx, client.ObjectKey{Name: req.Connection}, &gc); err != nil {
			g.kubeError(w, r, p, "git.check", req.Connection, connectionNotFound(req.Connection), err)
			return
		}
		if req.Project != "" && !controllers.ProjectAllowed(&gc, req.Project) {
			writeJSON(w, http.StatusOK, checkResult{Message: "The connection " + gc.Name + " is not available to the project " + req.Project + "."})
			return
		}
		if conn, err = g.connectionFor(ctx, &gc); err != nil {
			g.internalError(w, r, err)
			return
		}
		if err := conn.Covers(repo); err != nil {
			writeJSON(w, http.StatusOK, checkResult{Message: strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."})
			return
		}
	} else {
		var list kwerftv1.GitConnectionList
		if err := g.api.list(ctx, c, &list); err != nil {
			g.kubeError(w, r, p, "git.check", "", "Git connections not found.", err)
			return
		}
		slices.SortFunc(list.Items, func(a, b kwerftv1.GitConnection) int { return strings.Compare(a.Name, b.Name) })
		for i := range list.Items {
			gc := &list.Items[i]
			if !connectionView(gc).Ready || (req.Project != "" && !controllers.ProjectAllowed(gc, req.Project)) ||
				git.ConnectionFor(gc, nil).Covers(repo) != nil {
				continue
			}
			if conn, err = g.connectionFor(ctx, gc); err != nil {
				g.internalError(w, r, err)
				return
			}
			break
		}
	}
	provider, err := g.git.For(conn)
	if err != nil {
		writeJSON(w, http.StatusOK, checkResult{Message: err.Error(), Connection: conn.Name})
		return
	}
	ctx, cancelHost := context.WithTimeout(ctx, 20*time.Second)
	defer cancelHost()
	res := checkResult{Connection: conn.Name}
	def, err := provider.DefaultBranch(ctx, repo)
	if err != nil {
		res.Message = hostMessage(err, repo, conn, "Repository "+repo.String())
		writeJSON(w, http.StatusOK, res)
		return
	}
	res.DefaultBranch = def
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = def
	}
	head, err := provider.Head(ctx, repo, branch)
	if err != nil {
		res.Message = hostMessage(err, repo, conn, "Branch "+branch)
		writeJSON(w, http.StatusOK, res)
		return
	}
	if !git.ValidSHA(head.SHA) {
		res.Message = repo.Host + " did not answer with a commit SHA for " + branch + "."
		writeJSON(w, http.StatusOK, res)
		return
	}
	head.Message = shortSubject(head.Message)
	res.Head, res.OK = &head, true
	res.Message = "Found " + repo.String() + ", branch " + branch + " at " + head.SHA[:7]
	if head.Message != "" {
		res.Message += ": " + head.Message
	}
	res.Message += "."
	dockerfile := strings.TrimSpace(req.Dockerfile)
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	file := strings.TrimPrefix(path.Join("/", req.Path, dockerfile), "/")
	if found, err := provider.FileExists(ctx, repo, head.SHA, file); err == nil {
		res.Dockerfile = &found
		if !found {
			res.Message += " There is no " + file + "; Railpack can build it without one."
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func shortSubject(msg string) string {
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = strings.ToValidUTF8(msg[:200], "")
	}
	return msg
}

// buildNow is "Build now": the App's branch head (or a given commit), built
// and deployed. The Build is created as the user, so Kubernetes decides.
func (g *gitAPI) buildNow(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	var req struct {
		Commit string `json:"commit"`
	}
	if !decodeOptional(w, r, &req) {
		return
	}
	req.Commit = strings.ToLower(strings.TrimSpace(req.Commit))
	if req.Commit != "" && !git.ValidSHA(req.Commit) {
		writeFieldError(w, "commit", "Enter the full 40-character commit SHA.")
		return
	}
	c, p, ctx, cancel, err := g.userClient(r)
	defer cancel()
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	var app kwerftv1.App
	if err := c.Get(ctx, client.ObjectKey{Namespace: project, Name: name}, &app); err != nil {
		g.kubeError(w, r, p, "build.create", project+"/"+name, appNotFound(project, name), err)
		return
	}
	src := app.Spec.Source.Git
	if src == nil {
		writeError(w, http.StatusBadRequest, "This app runs an image from a registry; there is nothing to build.")
		return
	}
	repo, err := git.ParseRepository(src.Repository)
	if err != nil {
		writeFieldError(w, "repository", "The app's repository URL is not valid: "+err.Error()+".")
		return
	}
	conn := git.Public(repo)
	if src.Connection != "" {
		// Git connections live in the management cluster (docs/phase5.md).
		mc, err := g.managementClient(p)
		if err != nil {
			g.internalError(w, r, err)
			return
		}
		var gc kwerftv1.GitConnection
		if err := mc.Get(ctx, client.ObjectKey{Name: src.Connection}, &gc); err != nil {
			g.kubeError(w, r, p, "build.create", project+"/"+name, connectionNotFound(src.Connection), err)
			return
		}
		if !controllers.ProjectAllowed(&gc, project) {
			writeError(w, http.StatusForbidden, "The Git connection "+gc.Name+" is not available to the project "+project+".")
			return
		}
		if conn, err = g.connectionFor(ctx, &gc); err != nil {
			g.internalError(w, r, err)
			return
		}
	}
	branch := src.Branch
	if branch == "" {
		branch = "main"
	}
	commit := git.Commit{SHA: req.Commit}
	if provider, err := g.git.For(conn); err == nil {
		hostCtx, cancelHost := context.WithTimeout(ctx, 20*time.Second)
		if req.Commit == "" {
			commit, err = provider.Head(hostCtx, repo, branch)
			if err != nil {
				cancelHost()
				writeJSON(w, http.StatusBadGateway, map[string]string{"error": hostMessage(err, repo, conn, "Branch "+branch)})
				return
			}
		} else if found, err := provider.Commit(hostCtx, repo, req.Commit); err == nil {
			commit = found
		} else if errors.Is(err, git.ErrNotFound) {
			cancelHost()
			writeFieldError(w, "commit", "No commit "+builds.Short(req.Commit)+" in "+repo.String()+".")
			return
		}
		cancelHost()
	} else if req.Commit == "" {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !git.ValidSHA(commit.SHA) {
		writeError(w, http.StatusBadGateway, repo.Host+" did not answer with a commit SHA.")
		return
	}
	if req.Commit == "" {
		commit.SHA = strings.ToLower(commit.SHA)
	}
	b, err := builds.New(&app, builds.Request{
		Commit:  builds.Commit{SHA: commit.SHA, Branch: branch, Message: commit.Message, Author: commit.Author},
		Trigger: "manual", RequestedBy: p.user.Email, Deploy: true,
	})
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	if err := c.Create(ctx, b); err != nil {
		g.kubeError(w, r, p, "build.create", project+"/"+name, appNotFound(project, name), err)
		return
	}
	g.audit(r, p.user.Email, "build.create", project+"/"+b.Name, "manual build of "+builds.Short(commit.SHA)+" ("+branch+")")
	writeJSON(w, http.StatusCreated, buildSummary(b, &app, g.now()))
}
