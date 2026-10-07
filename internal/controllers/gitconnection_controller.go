// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/git"
)

// Git connections. The reconciler
//   - creates the credentials Secret git-<name> in kwerft-builds, with a
//     generated webhook secret, owned by the connection (a namespaced object
//     may have a cluster-scoped owner), so deleting the connection
//     garbage-collects it; it also deletes it itself when it finds the
//     connection gone;
//   - keeps the Role kwerft:git-credentials in kwerft-builds listing exactly
//     these Secrets with the verb patch, bound to owners and admins. That is
//     the DNS token's pattern for Secrets whose names are not known in
//     advance: the console writes credentials as the signed-in user
//     (impersonated), Kubernetes decides, and nobody — not even owners — may
//     get, list or create Secrets there. RBAC cannot match names by prefix,
//     and an empty resourceNames list would mean every Secret, so the
//     reconciler writes the names one by one and leaves the Role without
//     rules when there are no connections;
//   - verifies the credentials (status.account), publishes the webhook URL,
//     and sets up webhooks where the credentials allow: the GitHub App's
//     own, or one per repository that Apps build from;
//   - checks again every ten minutes, so revoked tokens show up.
//
// It reads Secrets with the API reader (uncached): caching them would put
// every Secret of the cluster into the controller's memory.

const (
	// GitCredentialsRole is the Role (and RoleBinding) in the builds
	// namespace that lets owners and admins patch the credential Secrets.
	GitCredentialsRole = "kwerft:git-credentials"
	// LabelGitConnection marks a credentials Secret with its connection.
	LabelGitConnection = "kwerft.dev/git-connection"
	// AnnotationCredentialsUpdated is set by the console after it wrote
	// credentials, so the reconciler checks them again at once.
	AnnotationCredentialsUpdated = "kwerft.dev/credentials-updated"

	// WebhookPath is where the console receives a connection's webhooks.
	WebhookPath = "/api/v1/hooks/git/"

	gitRecheck = 10 * time.Minute
	gitRetry   = time.Minute
	// gitWaitForCredentials is how long a new connection may lack its
	// credentials before that is an error: the console writes them right
	// after creating it.
	gitWaitForCredentials = time.Minute
)

// GitConnectionReconciler verifies connections and manages their Secrets,
// RBAC and webhooks.
type GitConnectionReconciler struct {
	client.Client
	// APIReader reads Secrets; nil falls back to the client (tests).
	APIReader client.Reader
	// Git talks to Git hosts; nil means a default Factory.
	Git *git.Factory
	// ConsoleDomain is the --console-domain flag, used until ConsoleSettings
	// reports where the console is served.
	ConsoleDomain string
	// ScanHostKey records an SSH host key on first use; nil means
	// git.ScanHostKey.
	ScanHostKey func(ctx context.Context, addr string) (line, fingerprint string, err error)
	Now         func() time.Time
}

func (r *GitConnectionReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *GitConnectionReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *GitConnectionReconciler) factory() *git.Factory {
	if r.Git == nil {
		r.Git = &git.Factory{}
	}
	return r.Git
}

// ConsoleHost is where the console is served now: ConsoleSettings' status,
// else the flag.
func ConsoleHost(ctx context.Context, c client.Reader, fallback string) string {
	var s kwerftv1.ConsoleSettings
	if err := c.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err == nil && s.Status.ConsoleDomain != "" {
		return s.Status.ConsoleDomain
	}
	return fallback
}

// NewWebhookSecret is a random webhook secret (256 bits, URL-safe).
func NewWebhookSecret() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (r *GitConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var gc kwerftv1.GitConnection
	err := r.Get(ctx, req.NamespacedName, &gc)
	if apierrors.IsNotFound(err) {
		if err := r.deleteOrphanedSecret(ctx, req.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.syncRole(ctx)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !gc.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := r.syncRole(ctx); err != nil {
		return ctrl.Result{}, err
	}
	data, err := r.ensureSecret(ctx, &gc)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// The builds namespace does not exist (yet).
			return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
				setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, "NoBuildsNamespace",
					"The namespace "+builds.Namespace+" does not exist; the installer creates it.")
			}, gitRetry)
		case isTerminal(err):
			return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
				setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
			}, gitRetry)
		}
		return ctrl.Result{}, err
	}
	conn := git.ConnectionFor(&gc, data)

	if gc.Spec.Auth == kwerftv1.GitAuthSSHKey && len(conn.KnownHosts) == 0 && len(conn.SSHPrivateKey) > 0 {
		if line, err := r.recordHostKey(ctx, &gc); err == nil {
			conn.KnownHosts = []byte(line)
		} else {
			return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
				setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, "HostKeyUnknown",
					"Could not read the SSH host key of "+conn.Host()+" ("+err.Error()+"). Add the host's known_hosts line to the connection.")
			}, gitRetry)
		}
	}

	if missing := missingCredentials(&gc, conn); missing != "" {
		since := gc.CreationTimestamp.Time
		if t, err := time.Parse(time.RFC3339Nano, gc.Annotations[AnnotationCredentialsUpdated]); err == nil && t.After(since) {
			since = t
		}
		if r.now().Sub(since) < gitWaitForCredentials {
			return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
				setReady(&st.Conditions, gc.Generation, metav1.ConditionUnknown, "WaitingForCredentials", "Waiting for the "+missing+".")
			}, 2*time.Second)
		}
		return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
			st.Account = ""
			setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, "CredentialsMissing", "No "+missing+" is stored. Edit the connection and enter it.")
		}, 0)
	}

	provider, err := r.factory().For(conn)
	if err != nil {
		return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
			setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, "InvalidConnection", err.Error())
		}, 0)
	}
	account, err := provider.Verify(ctx)
	if err != nil {
		reason, msg, retry := "HostUnreachable", "Could not reach "+conn.Host()+": "+err.Error(), gitRetry
		switch {
		case errors.Is(err, git.ErrUnauthorized):
			reason, msg, retry = "CredentialsRejected", conn.Host()+" rejected the credentials ("+err.Error()+"). Enter new ones.", gitRecheck
		case errors.Is(err, git.ErrForbidden), errors.Is(err, git.ErrNotFound):
			reason, msg, retry = "CredentialsRejected", "The credentials cannot read the account or installation ("+err.Error()+").", gitRecheck
		}
		return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
			st.Account = ""
			setReady(&st.Conditions, gc.Generation, metav1.ConditionFalse, reason, msg)
		}, retry)
	}

	host := ConsoleHost(ctx, r.Client, r.ConsoleDomain)
	hookURL := ""
	if host != "" {
		hookURL = "https://" + host + WebhookPath + gc.Name
	}
	repos, err := r.repositories(ctx, &gc, conn)
	if err != nil {
		return ctrl.Result{}, err
	}
	hooks := r.webhooks(ctx, &gc, conn, provider, git.Hook{URL: hookURL, Secret: string(data[builds.KeyWebhookSecret])}, repos)

	return r.report(ctx, &gc, func(st *kwerftv1.GitConnectionStatus) {
		st.Account = account
		st.WebhookURL = hookURL
		st.WebhookAutomatic = hooks.automatic
		st.Webhooks = hooks.repos
		if hooks.fingerprint != "" {
			st.WebhookFingerprint = hooks.fingerprint
		}
		msg := "Connected"
		if account != "" {
			msg += " as " + account
		}
		msg += "."
		if conn.Auth == kwerftv1.GitAuthNone {
			msg = "Public repositories over HTTPS; no commit checks."
		}
		if hooks.message != "" {
			msg += " " + hooks.message
		}
		setReady(&st.Conditions, gc.Generation, metav1.ConditionTrue, "Connected", msg)
	}, gitRecheck)
}

// missingCredentials names what the connection's auth needs but lacks.
func missingCredentials(gc *kwerftv1.GitConnection, c git.Connection) string {
	switch gc.Spec.Auth {
	case kwerftv1.GitAuthToken:
		if c.Token == "" {
			return "access token"
		}
	case kwerftv1.GitAuthSSHKey:
		if len(c.SSHPrivateKey) == 0 {
			return "SSH private key"
		}
	case kwerftv1.GitAuthGitHubApp:
		if len(c.GitHubAppKey) == 0 {
			return "GitHub App private key"
		}
	}
	return ""
}

// report writes the status if it changed, then requeues after.
func (r *GitConnectionReconciler) report(ctx context.Context, gc *kwerftv1.GitConnection, change func(*kwerftv1.GitConnectionStatus), after time.Duration) (ctrl.Result, error) {
	before := gc.DeepCopy()
	gc.Status.ObservedGeneration = gc.Generation
	change(&gc.Status)
	if !equality.Semantic.DeepEqual(before.Status, gc.Status) {
		if err := r.Status().Patch(ctx, gc, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// ---- the credentials Secret ------------------------------------------------------

// ensureSecret creates the connection's Secret with a webhook secret, or
// adds one to it, and returns its data.
func (r *GitConnectionReconciler) ensureSecret(ctx context.Context, gc *kwerftv1.GitConnection) (map[string][]byte, error) {
	key := client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(gc.Name)}
	var sec corev1.Secret
	err := r.reader().Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				Labels: map[string]string{LabelManagedBy: ManagedByKwerft, LabelGitConnection: gc.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kwerftv1.GroupVersion.String(), Kind: "GitConnection", Name: gc.Name, UID: gc.UID,
					Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true),
				}},
			},
			Type: corev1.SecretTypeOpaque,
			Data: map[string][]byte{builds.KeyWebhookSecret: []byte(NewWebhookSecret())},
		}
		if err := r.Create(ctx, &sec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("secret %s appeared meanwhile; retrying", key.Name)
			}
			return nil, err
		}
		log.FromContext(ctx).Info("created Git credentials secret", "secret", key.Name)
		return sec.Data, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&sec, gc) {
		// Never adopt credentials: a Secret left by an earlier connection of
		// the same name (not yet garbage-collected) holds credentials meant
		// for that one, perhaps for another host. Remove it; the next pass
		// creates a fresh one.
		if owner := metav1.GetControllerOf(&sec); owner == nil || owner.Kind != "GitConnection" || sec.Labels[LabelGitConnection] != gc.Name {
			return nil, terminalf("SecretConflict", "The Secret %s/%s exists and does not belong to this connection.", key.Namespace, key.Name)
		}
		if err := r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		return nil, fmt.Errorf("removed %s left by an earlier connection; recreating it", key.Name)
	}
	if len(sec.Data[builds.KeyWebhookSecret]) == 0 {
		// resourceVersion makes this a compare-and-swap: should the console
		// write its own webhook secret meanwhile, this patch fails instead of
		// replacing the one the console showed the user.
		secret := NewWebhookSecret()
		patch := map[string]any{
			"metadata": map[string]any{"resourceVersion": sec.ResourceVersion},
			"data":     map[string]string{builds.KeyWebhookSecret: base64.StdEncoding.EncodeToString([]byte(secret))},
		}
		if err := r.mergePatch(ctx, &sec, patch); err != nil {
			return nil, err
		}
		if sec.Data == nil {
			sec.Data = map[string][]byte{}
		}
		sec.Data[builds.KeyWebhookSecret] = []byte(secret)
	}
	return sec.Data, nil
}

func (r *GitConnectionReconciler) mergePatch(ctx context.Context, obj client.Object, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return r.Patch(ctx, obj, client.RawPatch(types.MergePatchType, raw))
}

func ptrTo[T any](v T) *T { return &v }

// deleteOrphanedSecret removes the Secret of a connection that is gone
// (garbage collection does too, but not at once).
func (r *GitConnectionReconciler) deleteOrphanedSecret(ctx context.Context, name string) error {
	var sec corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)}, &sec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	owner := metav1.GetControllerOf(&sec)
	if owner == nil || owner.Kind != "GitConnection" || owner.Name != name || sec.Labels[LabelGitConnection] != name {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}))
}

// recordHostKey scans the SSH host key of the connection's host (trust on
// first use) and stores it as known_hosts.
func (r *GitConnectionReconciler) recordHostKey(ctx context.Context, gc *kwerftv1.GitConnection) (string, error) {
	scan := r.ScanHostKey
	if scan == nil {
		scan = git.ScanHostKey
	}
	host := git.ConnectionFor(gc, nil).Host()
	line, fp, err := scan(ctx, net.JoinHostPort(host, "22"))
	if err != nil {
		return "", err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: builds.CredentialsSecret(gc.Name)}}
	if err := r.mergePatch(ctx, sec, map[string]any{"data": map[string]string{
		builds.KeyKnownHosts: base64.StdEncoding.EncodeToString([]byte(line + "\n")),
	}}); err != nil {
		return "", err
	}
	log.FromContext(ctx).Info("recorded SSH host key on first use", "host", host, "fingerprint", fp)
	return line, nil
}

// syncRole lists every connection's Secret in the credentials Role.
func (r *GitConnectionReconciler) syncRole(ctx context.Context) error {
	var list kwerftv1.GitConnectionList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	var names []string
	for _, gc := range list.Items {
		if gc.DeletionTimestamp.IsZero() {
			names = append(names, builds.CredentialsSecret(gc.Name))
		}
	}
	sort.Strings(names)
	role := rbacv1ac.Role(GitCredentialsRole, builds.Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft})
	if len(names) > 0 {
		// Never an empty resourceNames: that would mean every Secret.
		role = role.WithRules(rbacv1ac.PolicyRule().
			WithAPIGroups("").WithResources("secrets").WithVerbs("patch").WithResourceNames(names...))
	}
	if err := apply(ctx, r.Client, role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no builds namespace yet; reported with the Secret
		}
		return err
	}
	binding := rbacv1ac.RoleBinding(GitCredentialsRole, builds.Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("Role").WithName(GitCredentialsRole)).
		WithSubjects(
			rbacv1ac.Subject().WithAPIGroup(rbacv1.GroupName).WithKind(rbacv1.GroupKind).WithName("kwerft:role:owner"),
			rbacv1ac.Subject().WithAPIGroup(rbacv1.GroupName).WithKind(rbacv1.GroupKind).WithName("kwerft:role:admin"),
		)
	return apply(ctx, r.Client, binding)
}

// ---- webhooks ------------------------------------------------------------------------

// repositories are the distinct repositories that Apps using this
// connection build from, in the projects the connection serves.
func (r *GitConnectionReconciler) repositories(ctx context.Context, gc *kwerftv1.GitConnection, conn git.Connection) ([]git.Repo, error) {
	var apps kwerftv1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []git.Repo
	for _, app := range apps.Items {
		src := app.Spec.Source.Git
		if src == nil || src.Connection != gc.Name || !ProjectAllowed(gc, app.Namespace) {
			continue
		}
		repo, err := git.ParseRepository(src.Repository)
		if err != nil || conn.Covers(repo) != nil || seen[repo.Key()] {
			continue
		}
		seen[repo.Key()] = true
		out = append(out, repo)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out, nil
}

// ProjectAllowed says whether a project may build with the connection.
func ProjectAllowed(gc *kwerftv1.GitConnection, project string) bool {
	return len(gc.Spec.Projects) == 0 || slices.Contains(gc.Spec.Projects, project)
}

type webhookResult struct {
	automatic   bool
	repos       []kwerftv1.GitWebhookStatus
	fingerprint string // set when every automatic webhook is current
	message     string
}

// webhookFingerprint identifies what the hooks were last set to.
func webhookFingerprint(h git.Hook) string {
	sum := sha256.Sum256([]byte("kwerft-webhook\x00" + h.URL + "\x00" + h.Secret))
	return hex.EncodeToString(sum[:8])
}

func (r *GitConnectionReconciler) webhooks(ctx context.Context, gc *kwerftv1.GitConnection, conn git.Connection, p git.Provider, hook git.Hook, repos []git.Repo) webhookResult {
	var res webhookResult
	manual := func(msg string) webhookResult {
		for _, repo := range repos {
			res.repos = append(res.repos, kwerftv1.GitWebhookStatus{Repository: repo.String(), Message: msg})
		}
		return res
	}
	switch {
	case hook.URL == "":
		res.message = "The console's hostname is not known yet, so there is no webhook URL."
		return manual(res.message)
	case conn.Provider == kwerftv1.Generic || conn.Auth == kwerftv1.GitAuthNone || conn.Auth == kwerftv1.GitAuthSSHKey:
		return manual("Add the webhook by hand: push events, to this connection's webhook URL, with its secret.")
	}
	fp := webhookFingerprint(hook)
	update := gc.Status.WebhookFingerprint != fp
	logger := log.FromContext(ctx)

	if !update && gc.Status.WebhookAutomatic && conn.Auth == kwerftv1.GitAuthGitHubApp {
		res.automatic, res.fingerprint = true, fp
		return manual("") // the App's webhook covers every repository
	}
	if handled, err := p.ConnectionWebhook(ctx, hook); handled {
		if err != nil {
			logger.Info("could not set the GitHub App's webhook", "connection", gc.Name, "err", err.Error())
			res.message = "Could not set the GitHub App's webhook (" + err.Error() + "); set its URL and secret in the App's settings."
			return manual(res.message)
		}
		res.automatic, res.fingerprint = true, fp
		return manual("")
	}

	res.automatic = true
	for _, repo := range repos {
		st := kwerftv1.GitWebhookStatus{Repository: repo.String(), Automatic: true}
		if err := p.EnsureWebhook(ctx, repo, hook, update); err != nil {
			logger.Info("could not create a repository webhook", "connection", gc.Name, "repository", repo.String(), "err", err.Error())
			st.Automatic = false
			st.Message = "Could not create the webhook (" + err.Error() + "). Give the token permission to manage webhooks, or add it by hand."
			res.automatic = false
		}
		res.repos = append(res.repos, st)
	}
	if res.automatic {
		res.fingerprint = fp
	} else {
		res.message = "Some repositories need their webhook added by hand."
	}
	return res
}

// ---- wiring -----------------------------------------------------------------------------

func (r *GitConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Apps name their connection; a new repository needs its webhook.
	toConnection := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		app, ok := o.(*kwerftv1.App)
		if !ok || app.Spec.Source.Git == nil || app.Spec.Source.Git.Connection == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: app.Spec.Source.Git.Connection}}}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("gitconnection").
		// Status writes (lastDelivery, account) must not trigger a recheck.
		For(&kwerftv1.GitConnection{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&kwerftv1.App{}, toConnection, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
