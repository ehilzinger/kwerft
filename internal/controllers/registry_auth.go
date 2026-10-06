package controllers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"golang.org/x/crypto/bcrypt"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

const (
	// RegistryAuthSecret, in builds.RegistryNamespace, is what zot runs
	// with: config.json (the chart's ConfigMap kwerft-registry plus
	// authentication and access control) and htpasswd. The chart mounts it
	// into zot, which reloads both files when they change.
	RegistryAuthSecret = "kwerft-registry-auth"
	// RegistryAdminSecret, in builds.RegistryNamespace, is Kwerft's own
	// registry credential (user RegistryAdminUser): the keep tags of
	// RegistryKeeper and their removal.
	RegistryAdminSecret = "kwerft-registry-admin"
	RegistryAdminUser   = "kwerft"
	// LabelRegistryCredential marks the projects' credential Secrets in
	// builds.Namespace; its value is the project.
	LabelRegistryCredential = "kwerft.dev/registry-credential"
	// AnnotationRegistryActive on a project's credential Secret is the
	// fingerprint (registryFingerprint) of the credential zot was seen to
	// accept. zot learns a new credential when the kubelet updates its
	// Secret volume, up to a minute or so later; builds wait for it.
	AnnotationRegistryActive = "kwerft.dev/registry-active"

	registryConfigKey   = "config.json"
	registryHTPasswdKey = "htpasswd"
	// registryHTPasswdPath is where zot reads htpasswd: the chart mounts
	// RegistryAuthSecret at /etc/zot.
	registryHTPasswdPath = "/etc/zot/htpasswd"
	// registryLegacyRecheck: while builds started before the registry
	// required credentials still run, look again this often (their Jobs'
	// watch is the fast path).
	registryLegacyRecheck = time.Minute
	// registryProbeRecheck: while zot does not accept a new credential yet,
	// ask again this often.
	registryProbeRecheck = 5 * time.Second
	// registryBcryptCost: zot checks the password on every request a
	// client authenticates. The passwords are 256 random bits, which no
	// cost factor makes any harder to guess, so the cheapest bcrypt keeps
	// pushes fast.
	registryBcryptCost = bcrypt.MinCost
)

// What zot lets a project's user do in the project's repositories (push and
// re-tag, e.g. the build cache), and Kwerft's own user everywhere.
var (
	registryProjectActions = []string{"read", "create", "update"}
	registryAdminActions   = []string{"read", "create", "update", "delete"}
	registryAnonymous      = []string{"read"}
	// registryName is what may appear in an access-control pattern:
	// project and App names are DNS labels.
	registryName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// RegistryAuthReconciler keeps the in-cluster registry's (zot's) users and
// access control (docs/phase2.md › Registry credentials):
//
//   - every project gets a random credential, the Secret
//     builds.RegistrySecret(project) in kwerft-builds, which only that
//     project's build pods mount: its user may read, push and re-tag in
//     <project>/** and nothing else;
//   - Kwerft's own credential (RegistryAdminSecret) may also delete, for
//     RegistryKeeper's keep tags;
//   - anyone may read (pull) without a credential: the nodes' containerd
//     pulls through the k3s mirror without one;
//   - nobody may push without one.
//
// It renders zot's config.json (the chart's ConfigMap plus "auth" and
// "accessControl") and htpasswd into RegistryAuthSecret. zot reloads both
// files when the Secret changes, so a new project needs no restart. A
// project's Secret goes with the project (owner reference, and removed here
// as well).
//
// zot sees a changed Secret only once the kubelet has updated the volume,
// so a project's credential is marked active (AnnotationRegistryActive)
// once Probe finds zot accepting it, and builds wait for that.
//
// Builds started before the registry required credentials (Jobs without
// builds.LabelRegistryAuth) may push anonymously to their own repository
// while they run, so an upgrade does not fail the builds in flight.
type RegistryAuthReconciler struct {
	client.Client
	// APIReader reads Secrets and the registry's ConfigMap from the API
	// server: Kwerft does not cache Secrets, nor every ConfigMap.
	APIReader client.Reader
	// Probe reports whether zot accepts a project's credential yet
	// (RegistryProbe); nil takes every credential as accepted (tests
	// without a registry).
	Probe func(ctx context.Context, project, user, password string) (bool, error)
}

// registryKey is the one request this reconciler handles: the whole registry.
var registryKey = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: builds.RegistryNamespace, Name: RegistryAuthSecret}}

func (r *RegistryAuthReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	var base corev1.ConfigMap
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: builds.RegistryService}, &base); err != nil {
		// No registry in this cluster (chart: registry.enabled=false); the
		// ConfigMap's watch brings us back when there is one.
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	admin, _, err := r.ensureCredential(ctx, builds.RegistryNamespace, RegistryAdminSecret, RegistryAdminUser, nil)
	if err != nil {
		return ctrl.Result{}, err
	}
	lines := []string{admin.line}
	// Project credentials zot has not been seen to accept yet.
	pending := map[*kwerftv1.Project]registryCredential{}

	// Project credentials live where the build pods run; without
	// kwerft-builds (chart: builds.enabled=false) nothing pushes.
	var projects []string
	switch err := r.Get(ctx, client.ObjectKey{Name: builds.Namespace}, &corev1.Namespace{}); {
	case err == nil:
		var list kwerftv1.ProjectList
		if err := r.List(ctx, &list); err != nil {
			return ctrl.Result{}, err
		}
		for i := range list.Items {
			p := &list.Items[i]
			if !p.DeletionTimestamp.IsZero() || !registryName.MatchString(p.Name) {
				continue
			}
			cred, active, err := r.ensureCredential(ctx, builds.Namespace, builds.RegistrySecret(p.Name), builds.RegistryUser(p.Name), p)
			if err != nil {
				return ctrl.Result{}, err
			}
			if !active {
				pending[p] = cred
			}
			lines = append(lines, cred.line)
			projects = append(projects, p.Name)
		}
		if err := r.removeStale(ctx, projects); err != nil {
			return ctrl.Result{}, err
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	legacy, err := r.legacyRepositories(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	config, err := renderRegistryConfig([]byte(base.Data[registryConfigKey]), projects, legacy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ConfigMap %s/%s: %w", builds.RegistryNamespace, builds.RegistryService, err)
	}
	if err := r.writeAuth(ctx, config, renderHTPasswd(lines)); err != nil {
		return ctrl.Result{}, err
	}
	var res ctrl.Result
	if len(legacy) > 0 {
		res.RequeueAfter = registryLegacyRecheck
	}
	for p, cred := range pending {
		ok, err := r.accepted(ctx, p.Name, cred)
		if err != nil {
			log.FromContext(ctx).Info("registry: cannot check a credential yet", "project", p.Name, "err", err.Error())
		}
		if !ok {
			res.RequeueAfter = registryProbeRecheck
			continue
		}
		if err := r.writeCredential(ctx, builds.Namespace, builds.RegistrySecret(p.Name), cred, p, true); err != nil {
			return ctrl.Result{}, err
		}
	}
	return res, nil
}

func (r *RegistryAuthReconciler) accepted(ctx context.Context, project string, c registryCredential) (bool, error) {
	if r.Probe == nil {
		return true, nil
	}
	return r.Probe(ctx, project, c.user, c.password)
}

// registryFingerprint identifies a credential (its htpasswd line) without
// revealing it.
func registryFingerprint(line string) string {
	sum := sha256.Sum256([]byte(line))
	return hex.EncodeToString(sum[:])[:16]
}

// RegistryCredentialActive reports whether zot was seen to accept the
// credential in a project's Secret, so a build can push with it.
func RegistryCredentialActive(sec *corev1.Secret) bool {
	line := string(sec.Data[builds.KeyRegistryHTPasswd])
	return line != "" && len(sec.Data[corev1.DockerConfigJsonKey]) > 0 &&
		sec.Annotations[AnnotationRegistryActive] == registryFingerprint(line)
}

// RegistryProbe asks zot at url whether it accepts a project's credential:
// an authenticated read in the project's repositories, which zot answers
// with 404 (no such repository) once it knows the user and the project's
// access, and with 401 or 403 before.
func RegistryProbe(url string, hc *http.Client) func(ctx context.Context, project, user, password string) (bool, error) {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return func(ctx context.Context, project, user, password string) (bool, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/v2/"+project+"/kwerft-probe/tags/list", nil)
		if err != nil {
			return false, err
		}
		req.SetBasicAuth(user, password)
		resp, err := hc.Do(req)
		if err != nil {
			return false, err
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK, http.StatusNotFound:
			return true, nil
		case http.StatusUnauthorized, http.StatusForbidden:
			return false, nil
		}
		return false, fmt.Errorf("registry: %s", resp.Status)
	}
}

// registryCredential is one zot user.
type registryCredential struct {
	user, password string
	line           string // user:bcrypt-hash
}

func newRegistryCredential(user string) (registryCredential, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return registryCredential{}, err
	}
	password := base64.RawURLEncoding.EncodeToString(buf)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), registryBcryptCost)
	if err != nil {
		return registryCredential{}, err
	}
	return registryCredential{user: user, password: password, line: user + ":" + string(hash)}, nil
}

// credentialFrom reads a credential Secret; ok is false when it is not a
// complete, consistent credential of user.
func credentialFrom(data map[string][]byte, user string) (registryCredential, bool) {
	c := registryCredential{user: user, password: string(data[builds.KeyRegistryPassword]), line: string(data[builds.KeyRegistryHTPasswd])}
	hash, ok := strings.CutPrefix(c.line, user+":")
	if string(data[builds.KeyRegistryUsername]) != user || c.password == "" || !ok {
		return c, false
	}
	return c, bcrypt.CompareHashAndPassword([]byte(hash), []byte(c.password)) == nil
}

// dockerConfig is BuildKit's view of a credential: a Docker config.json
// for the registry's name in image references.
func dockerConfig(c registryCredential) []byte {
	auth := base64.StdEncoding.EncodeToString([]byte(c.user + ":" + c.password))
	b, _ := json.Marshal(map[string]any{"auths": map[string]any{
		builds.RegistryHost: map[string]string{"username": c.user, "password": c.password, "auth": auth},
	}})
	return b
}

// ensureCredential returns the credential in Secret namespace/name, making
// a new one if it is missing or broken, and whether zot was seen to accept
// it (project credentials only). A project's (project != nil) is a Docker
// config Secret the project owns.
func (r *RegistryAuthReconciler) ensureCredential(ctx context.Context, namespace, name, user string, project *kwerftv1.Project) (registryCredential, bool, error) {
	var sec corev1.Secret
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &sec)
	if err != nil && !apierrors.IsNotFound(err) {
		return registryCredential{}, false, err
	}
	exists := err == nil
	cred, ok := credentialFrom(sec.Data, user)
	if !ok {
		if cred, err = newRegistryCredential(user); err != nil {
			return registryCredential{}, false, err
		}
	}
	active := ok && sec.Annotations[AnnotationRegistryActive] == registryFingerprint(cred.line)
	if want := credentialSecret(namespace, name, cred, project, active); exists && credentialUpToDate(&sec, want) {
		return cred, active, nil
	}
	if exists && sec.Type != credentialType(project) {
		// The type cannot change in place.
		if err := r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}); client.IgnoreNotFound(err) != nil {
			return registryCredential{}, false, err
		}
	}
	if err := r.writeCredential(ctx, namespace, name, cred, project, active); err != nil {
		return registryCredential{}, false, err
	}
	return cred, active, nil
}

func credentialType(project *kwerftv1.Project) corev1.SecretType {
	if project != nil {
		return corev1.SecretTypeDockerConfigJson
	}
	return corev1.SecretTypeOpaque
}

// credentialSecret is the Secret that holds cred.
func credentialSecret(namespace, name string, cred registryCredential, project *kwerftv1.Project, active bool) *corev1ac.SecretApplyConfiguration {
	data := map[string][]byte{
		builds.KeyRegistryUsername: []byte(cred.user),
		builds.KeyRegistryPassword: []byte(cred.password),
		builds.KeyRegistryHTPasswd: []byte(cred.line),
	}
	labels := map[string]string{LabelManagedBy: ManagedByKwerft}
	ac := corev1ac.Secret(name, namespace).WithType(credentialType(project))
	if project != nil {
		data[corev1.DockerConfigJsonKey] = dockerConfig(cred)
		labels[LabelRegistryCredential] = project.Name
		labels[LabelProject] = project.Name
		ac.WithOwnerReferences(controllerRef(project, kwerftv1.GroupVersion.WithKind("Project")))
		if active {
			ac.WithAnnotations(map[string]string{AnnotationRegistryActive: registryFingerprint(cred.line)})
		}
	}
	return ac.WithLabels(labels).WithData(data)
}

// credentialUpToDate compares what Kwerft writes of a credential Secret.
func credentialUpToDate(sec *corev1.Secret, want *corev1ac.SecretApplyConfiguration) bool {
	if sec.Type != *want.Type || !maps.EqualFunc(sec.Data, want.Data, bytes.Equal) {
		return false
	}
	for k, v := range want.Labels {
		if sec.Labels[k] != v {
			return false
		}
	}
	if sec.Annotations[AnnotationRegistryActive] != want.Annotations[AnnotationRegistryActive] {
		return false
	}
	for _, o := range want.OwnerReferences {
		if !slices.ContainsFunc(sec.OwnerReferences, func(have metav1.OwnerReference) bool { return have.UID == *o.UID }) {
			return false
		}
	}
	return true
}

func (r *RegistryAuthReconciler) writeCredential(ctx context.Context, namespace, name string, cred registryCredential, project *kwerftv1.Project, active bool) error {
	if err := apply(ctx, r.Client, credentialSecret(namespace, name, cred, project, active)); err != nil {
		return fmt.Errorf("write registry credential %s/%s: %w", namespace, name, err)
	}
	return nil
}

// removeStale deletes the credentials of projects that are gone (garbage
// collection does too, through the owner reference).
func (r *RegistryAuthReconciler) removeStale(ctx context.Context, projects []string) error {
	var list metav1.PartialObjectMetadataList
	list.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("SecretList"))
	if err := r.List(ctx, &list, client.InNamespace(builds.Namespace), client.HasLabels{LabelRegistryCredential}); err != nil {
		return err
	}
	for i := range list.Items {
		s := &list.Items[i]
		if p := s.Labels[LabelRegistryCredential]; slices.Contains(projects, p) && s.Name == builds.RegistrySecret(p) {
			continue
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: s.Name}}
		if err := r.Delete(ctx, sec, client.Preconditions{UID: &s.UID}); client.IgnoreNotFound(err) != nil && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

// legacyRepositories lists the repositories (<project>/<app>) of builds
// still running that were started before builds pushed with a credential.
func (r *RegistryAuthReconciler) legacyRepositories(ctx context.Context) ([]string, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(builds.Namespace), client.HasLabels{builds.LabelBuild}); err != nil {
		return nil, err
	}
	var repos []string
	for i := range jobs.Items {
		j := &jobs.Items[i]
		if j.Labels[builds.LabelRegistryAuth] != "" || !j.DeletionTimestamp.IsZero() || jobFinished(j) {
			continue
		}
		if repo := jobRepository(j); repo != "" {
			repos = append(repos, repo)
		}
	}
	slices.Sort(repos)
	return slices.Compact(repos), nil
}

// jobRepository is the repository (<project>/<app>) a build Job pushes to,
// from its build container's KWERFT_IMAGE; "" unless that is in the Job's
// own project.
func jobRepository(j *batchv1.Job) string {
	for _, c := range j.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name != "KWERFT_IMAGE" {
				continue
			}
			ref, ok := strings.CutPrefix(e.Value, builds.RegistryHost+"/")
			if !ok {
				return ""
			}
			ref, _, _ = strings.Cut(ref, "@")
			ref, _, _ = strings.Cut(ref, ":")
			project, app, ok := strings.Cut(ref, "/")
			if !ok || project != j.Labels[LabelProject] || !registryName.MatchString(project) || !registryName.MatchString(app) {
				return ""
			}
			return ref
		}
	}
	return ""
}

func (r *RegistryAuthReconciler) writeAuth(ctx context.Context, config, htpasswd []byte) error {
	data := map[string][]byte{registryConfigKey: config, registryHTPasswdKey: htpasswd}
	var sec corev1.Secret
	switch err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: RegistryAuthSecret}, &sec); {
	case err == nil:
		if maps.EqualFunc(sec.Data, data, bytes.Equal) && sec.Labels[LabelManagedBy] == ManagedByKwerft {
			return nil
		}
	case !apierrors.IsNotFound(err):
		return err
	}
	ac := corev1ac.Secret(RegistryAuthSecret, builds.RegistryNamespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithType(corev1.SecretTypeOpaque).
		WithData(data)
	if err := apply(ctx, r.Client, ac); err != nil {
		return fmt.Errorf("write %s: %w", RegistryAuthSecret, err)
	}
	return nil
}

// renderHTPasswd is zot's user file, one user:hash line each, sorted so it
// only changes when a user does.
func renderHTPasswd(lines []string) []byte {
	lines = slices.Clone(lines)
	slices.Sort(lines)
	return []byte(strings.Join(lines, "\n") + "\n")
}

// policyGroup is one zot accessControl.repositories entry.
func policyGroup(users []string, anonymous []string) map[string]any {
	g := map[string]any{"anonymousPolicy": anonymous}
	if len(users) > 0 {
		g["policies"] = []any{map[string]any{"users": users, "actions": registryProjectActions}}
	}
	return g
}

// renderRegistryConfig adds authentication and access control to zot's
// base configuration (the chart's ConfigMap). zot decides by the longest
// pattern that matches a repository: "**" lets everyone read and nobody
// push; "<project>/**" adds the project's user; a legacy build's own
// repository ("<project>/<app>") also takes anonymous pushes while it runs.
// The admin policy (Kwerft's user) applies everywhere.
func renderRegistryConfig(base []byte, projects, legacy []string) ([]byte, error) {
	var cfg map[string]any
	dec := json.NewDecoder(bytes.NewReader(base))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("zot's config.json: %w", err)
	}
	if cfg == nil {
		return nil, errors.New("zot's config.json is empty")
	}
	http, _ := cfg["http"].(map[string]any)
	if http == nil {
		http = map[string]any{}
	}
	repos := map[string]any{"**": policyGroup(nil, registryAnonymous)}
	for _, p := range projects {
		if registryName.MatchString(p) {
			repos[p+"/**"] = policyGroup([]string{builds.RegistryUser(p)}, registryAnonymous)
		}
	}
	for _, repo := range legacy {
		p, app, _ := strings.Cut(repo, "/")
		if !registryName.MatchString(p) || !registryName.MatchString(app) {
			continue
		}
		var users []string
		if slices.Contains(projects, p) {
			users = []string{builds.RegistryUser(p)}
		}
		// "<project>/**/<app>" matches the repository itself and is always
		// longer than "<project>/**", which "<project>/<app>" is not for
		// App names of one or two letters.
		repos[p+"/**/"+app] = policyGroup(users, registryProjectActions)
	}
	http["auth"] = map[string]any{"htpasswd": map[string]any{"path": registryHTPasswdPath}}
	http["accessControl"] = map[string]any{
		"repositories": repos,
		"adminPolicy":  map[string]any{"users": []string{RegistryAdminUser}, "actions": registryAdminActions},
	}
	cfg["http"] = http
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// RegistryAdminCredentials reads Kwerft's own registry credential for
// RegistryKeeper. Without the Secret (a registry that predates
// credentials, or none yet) it returns no user, and requests go out
// anonymously.
func RegistryAdminCredentials(c client.Reader) func(context.Context) (string, string, error) {
	return func(ctx context.Context) (string, string, error) {
		var sec corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: RegistryAdminSecret}, &sec); err != nil {
			if apierrors.IsNotFound(err) {
				return "", "", nil
			}
			return "", "", err
		}
		return string(sec.Data[builds.KeyRegistryUsername]), string(sec.Data[builds.KeyRegistryPassword]), nil
	}
}

func (r *RegistryAuthReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{registryKey}
	})
	// The registry's own objects: its ConfigMap and the Secrets this
	// reconciler writes, so an edit or a deletion is put right.
	ours := predicate.NewPredicateFuncs(func(o client.Object) bool {
		switch o.GetNamespace() {
		case builds.RegistryNamespace:
			return o.GetName() == builds.RegistryService || o.GetName() == RegistryAuthSecret || o.GetName() == RegistryAdminSecret
		case builds.Namespace:
			return o.GetLabels()[LabelRegistryCredential] != ""
		}
		return false
	})
	// Build Jobs from before credentials: their end ends their exception.
	legacyJob := predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetNamespace() == builds.Namespace && o.GetLabels()[builds.LabelBuild] != "" && o.GetLabels()[builds.LabelRegistryAuth] == ""
	})
	buildsNamespace := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == builds.Namespace })
	return ctrl.NewControllerManagedBy(mgr).
		Named("registry").
		Watches(&kwerftv1.Project{}, one).
		Watches(&corev1.Namespace{}, one, builder.WithPredicates(buildsNamespace)).
		Watches(&corev1.ConfigMap{}, one, builder.OnlyMetadata, builder.WithPredicates(ours)).
		Watches(&corev1.Secret{}, one, builder.OnlyMetadata, builder.WithPredicates(ours)).
		Watches(&batchv1.Job{}, one, builder.WithPredicates(legacyJob)).
		Complete(r)
}
