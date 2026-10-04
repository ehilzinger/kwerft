package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

const (
	// MaxBuildsPerApp is how many Builds an App keeps; older finished ones
	// are deleted, except those a revision in the App's history runs.
	MaxBuildsPerApp = 20

	// queuePoll re-checks a queued build in case a freed slot went unnoticed.
	queuePoll = 15 * time.Second
)

// BuildReconciler runs each Build as a Job in kwerft-builds: it numbers the
// App's builds, queues them so at most MaxConcurrentBuilds run in the
// cluster, clones the commit with the GitConnection's credentials, builds it
// with rootless BuildKit (Dockerfile or Railpack) and pushes the image to the
// in-cluster registry. It reports phase, pod, image, digest and a short
// failure reason, cancels on request, and keeps the newest MaxBuildsPerApp
// Builds of each App.
//
// Jobs live in another namespace than their Build, so no owner reference
// connects them: Jobs carry kwerft.dev/build and kwerft.dev/project, and the
// reconciler deletes a Build's Job itself when the Build goes.
type BuildReconciler struct {
	client.Client
	// APIReader reads Jobs, pods and Secrets from the API server: the
	// numbering and the queue must not act on a stale cache, and Kwerft does
	// not cache Secrets.
	APIReader client.Reader

	BuildKitImage string
	RailpackImage string
	// AppArmorProfile is the Localhost profile build containers run under
	// (install.sh loads "kwerft-buildkit" on every node); empty runs them
	// unconfined, which Ubuntu's user-namespace restriction refuses.
	AppArmorProfile     string
	MaxConcurrentBuilds int
	Timeout             time.Duration

	// InstallationToken mints GitHub App tokens; nil uses the GitHub API.
	InstallationToken InstallationTokenFunc
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
	// Ignore leaves the Builds it matches alone; nil ignores none. Tests of
	// other reconcilers use it to drive Build status by hand.
	Ignore func(*kwerftv1.Build) bool

	// mu serialises numbering and starting builds, so two reconciles never
	// hand out the same number or the same free slot.
	mu sync.Mutex
}

func (r *BuildReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *BuildReconciler) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return DefaultBuildTimeout
}

func (r *BuildReconciler) slots() int {
	return max(1, r.MaxConcurrentBuilds)
}

func buildFinished(p kwerftv1.BuildPhase) bool {
	return p == kwerftv1.BuildSucceeded || p == kwerftv1.BuildFailed || p == kwerftv1.BuildCancelled
}

func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b kwerftv1.Build
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, r.cleanUpDeleted(ctx, req.Namespace, req.Name)
		}
		return ctrl.Result{}, err
	}
	if !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // the Job goes once the Build is gone
	}
	if r.Ignore != nil && r.Ignore(&b) {
		return ctrl.Result{}, nil
	}
	orig := b.DeepCopy()

	res, err := r.reconcile(ctx, &b)
	if err != nil {
		setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
		if isTerminal(err) && !buildFinished(b.Status.Phase) {
			b.Status.Phase = kwerftv1.BuildFailed // it cannot run; say so
			b.Status.CompletionTime = ptrTime(r.now())
			b.Status.Message = err.Error()
		}
	}
	if !equality.Semantic.DeepEqual(orig.Status, b.Status) {
		// A plain merge patch, not patchStatus: the reconciler reads what
		// numbering and the queue depend on from the API server, and it
		// patches the Build's metadata (owner) first, which a
		// resourceVersion guard would turn into a conflict on every start.
		if perr := r.Status().Patch(ctx, &b, client.MergeFrom(orig)); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if buildFinished(b.Status.Phase) {
		// Repeated on every reconcile of a finished build: both are cheap
		// and idempotent, and a failed attempt is retried.
		if ferr := r.finish(ctx, &b); ferr != nil {
			return ctrl.Result{}, ferr
		}
		return ctrl.Result{}, nil
	}
	if err != nil && !isTerminal(err) {
		return ctrl.Result{}, err
	}
	return res, nil
}

func (r *BuildReconciler) reconcile(ctx context.Context, b *kwerftv1.Build) (ctrl.Result, error) {
	if b.Status.Phase == "" {
		b.Status.Phase = kwerftv1.BuildPending
	}
	if b.Status.Number == 0 {
		if err := r.number(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}
	if buildFinished(b.Status.Phase) {
		return ctrl.Result{}, nil
	}
	if by := b.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		return ctrl.Result{}, r.cancel(ctx, b, by)
	}
	if b.Status.Job == "" {
		// A Job may exist while the cached Build does not show it yet.
		job := &batchv1.Job{}
		name := buildJobName(b.Namespace, b.Name)
		switch err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: name}, job); {
		case apierrors.IsNotFound(err):
			return r.start(ctx, b)
		case err != nil:
			return ctrl.Result{}, err
		case !ownsJob(b, job):
			return ctrl.Result{}, terminalf("JobConflict", "a Job named %q already exists in %s and does not belong to this build", name, builds.Namespace)
		}
		b.Status.Job = name
		// start() records these with the Job; if that status write was lost
		// (a conflict), they must not be.
		if b.Status.Image == "" {
			b.Status.Image = builds.ImageRef(b.Namespace, b.Spec.App, b.Spec.Commit)
		}
	}
	return r.observe(ctx, b)
}

// number gives the App's unnumbered builds the next numbers, oldest first.
// It reads the live list (not the cache), under the lock, so a number just
// handed out is always seen.
func (r *BuildReconciler) number(ctx context.Context, b *kwerftv1.Build) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list kwerftv1.BuildList
	if err := r.APIReader.List(ctx, &list, client.InNamespace(b.Namespace)); err != nil {
		return err
	}
	var last int64
	var unnumbered []*kwerftv1.Build
	for i := range list.Items {
		o := &list.Items[i]
		if o.Spec.App != b.Spec.App {
			continue
		}
		if o.Name == b.Name && o.Status.Number != 0 {
			b.Status.Number = o.Status.Number // numbered meanwhile; the cache lagged
			return nil
		}
		last = max(last, o.Status.Number)
		if o.Status.Number == 0 && o.Name != b.Name {
			unnumbered = append(unnumbered, o)
		}
	}
	unnumbered = append(unnumbered, b)
	slices.SortStableFunc(unnumbered, olderFirst)
	for i, o := range unnumbered {
		n := last + int64(i) + 1
		if o.Name == b.Name {
			b.Status.Number = n
			continue
		}
		patch := fmt.Appendf(nil, `{"status":{"number":%d}}`, n)
		if err := r.Status().Patch(ctx, o, client.RawPatch(types.MergePatchType, patch)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func olderFirst(a, b *kwerftv1.Build) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if c := strings.Compare(a.Namespace, b.Namespace); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

// cancel stops the build: its Job goes (and the pod with it) and the Build
// becomes Cancelled. A queued build never gets a Job.
func (r *BuildReconciler) cancel(ctx context.Context, b *kwerftv1.Build, by string) error {
	if err := r.deleteJobs(ctx, b.Namespace, b.Name); err != nil {
		return err
	}
	b.Status.Phase = kwerftv1.BuildCancelled
	b.Status.CompletionTime = ptrTime(r.now())
	b.Status.Message = "Cancelled by " + by
	setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Cancelled", b.Status.Message)
	return nil
}

// start runs the build if a slot is free, else queues it.
func (r *BuildReconciler) start(ctx context.Context, b *kwerftv1.Build) (ctrl.Result, error) {
	run, conn, wait, err := r.prepare(ctx, b)
	if err != nil {
		return ctrl.Result{}, err
	}
	if wait != nil {
		b.Status.Message = wait.message
		setReady(&b.Status.Conditions, b.Generation, wait.status, wait.reason, wait.message)
		return ctrl.Result{RequeueAfter: queuePoll}, nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	active, err := r.activeJobs(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	queue, err := r.queue(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	pos := slices.IndexFunc(queue, func(o *kwerftv1.Build) bool { return o.Namespace == b.Namespace && o.Name == b.Name })
	if pos < 0 {
		queue = append(queue, b) // not in the cache yet
		pos = len(queue) - 1
	}
	if len(active)+pos >= r.slots() {
		var ahead string
		switch {
		case pos > 0:
			ahead = r.describe(ctx, client.ObjectKeyFromObject(queue[pos-1]), b)
		case len(active) > 0:
			ahead = r.describeJob(ctx, &active[0], b)
		}
		msg := "Queued"
		if ahead != "" {
			msg = "Queued behind " + ahead
		}
		b.Status.Phase = kwerftv1.BuildPending
		b.Status.Message = msg
		setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Queued", msg)
		return ctrl.Result{RequeueAfter: queuePoll}, nil
	}

	var token string
	if conn != nil && conn.Spec.Auth == kwerftv1.GitAuthGitHubApp {
		if token, err = r.mintToken(ctx, conn); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := apply(ctx, r.Client, run.job()); err != nil {
		if apierrors.IsInvalid(err) {
			return ctrl.Result{}, terminalf("InvalidJob", "the build Job was rejected: %v", err)
		}
		return ctrl.Result{}, fmt.Errorf("apply job: %w", err)
	}
	b.Status.Job = run.jobName
	b.Status.Image = run.image
	b.Status.Message = ""
	setReady(&b.Status.Conditions, b.Generation, metav1.ConditionFalse, "Pending", "Job created")
	if token != "" {
		if err := r.writeToken(ctx, run, token); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

// writeToken stores the installation token in the Secret the clone mounts,
// owned by the Job. The pod waits for it to exist before it starts.
func (r *BuildReconciler) writeToken(ctx context.Context, run *buildRun, token string) error {
	job := &batchv1.Job{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: run.jobName}, job); err != nil {
		return err
	}
	if err := apply(ctx, r.Client, run.tokenSecret(token, string(job.UID))); err != nil {
		return fmt.Errorf("write token secret: %w", err)
	}
	return nil
}

func (r *BuildReconciler) mintToken(ctx context.Context, conn *kwerftv1.GitConnection) (string, error) {
	var sec corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(conn.Name)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", terminalf("CredentialsMissing", "the Git connection %s has no credentials yet", conn.Name)
		}
		return "", err
	}
	key := sec.Data[builds.KeyGitHubAppKey]
	if len(key) == 0 {
		return "", terminalf("CredentialsMissing", "the Git connection %s has no GitHub App private key", conn.Name)
	}
	mint := r.InstallationToken
	if mint == nil {
		mint = GitHubInstallationToken(nil)
	}
	token, err := mint(ctx, conn, key)
	if err != nil {
		var perm permanentError
		if errors.As(err, &perm) {
			return "", terminalf("GitHubAppFailed", "GitHub App %s: %v", conn.Name, perm.error)
		}
		return "", err
	}
	return token, nil
}

// prepare resolves the build against its App, GitConnection and the
// registry. A problem only the user can fix is a terminal error, which fails
// the build at once instead of letting it wait in the queue.
func (r *BuildReconciler) prepare(ctx context.Context, b *kwerftv1.Build) (*buildRun, *kwerftv1.GitConnection, *readiness, error) {
	var app kwerftv1.App
	if err := r.Get(ctx, client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.App}, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, nil, terminalf("AppNotFound", "App %s does not exist", b.Spec.App)
		}
		return nil, nil, nil, err
	}
	if err := r.ownBy(ctx, b, &app); err != nil {
		return nil, nil, nil, err
	}
	if _, err := projectOf(ctx, r.Client, b.Namespace); err != nil {
		return nil, nil, nil, err
	}

	src := b.Spec.Source
	run := &buildRun{
		build:         b,
		project:       b.Namespace,
		jobName:       buildJobName(b.Namespace, b.Name),
		image:         builds.ImageRef(b.Namespace, b.Spec.App, b.Spec.Commit),
		cache:         builds.CacheRef(b.Namespace, b.Spec.App),
		buildkitImage: orDefault(r.BuildKitImage, DefaultBuildKitImage),
		railpackImage: orDefault(r.RailpackImage, DefaultRailpackImage),
		appArmor:      r.AppArmorProfile,
		timeout:       r.timeout(),
		contextDir:    cleanRepoPath(src.Path),
		dockerfile:    cleanRepoPath(orDefault(src.Dockerfile, "Dockerfile")),
		auth:          "none",
	}
	transport := repoTransport(src.Repository)
	if transport == "" {
		return nil, nil, nil, terminalf("InvalidRepository", "%q is not an HTTPS or SSH Git URL", src.Repository)
	}

	var conn *kwerftv1.GitConnection
	if src.Connection == "" {
		if transport != "https" {
			return nil, nil, nil, terminalf("ConnectionRequired", "cloning over SSH needs a Git connection with a deploy key")
		}
	} else {
		conn = &kwerftv1.GitConnection{}
		if err := r.Get(ctx, client.ObjectKey{Name: src.Connection}, conn); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil, nil, terminalf("ConnectionNotFound", "Git connection %s does not exist", src.Connection)
			}
			return nil, nil, nil, err
		}
		if err := checkConnection(conn, b.Namespace, src.Repository, transport); err != nil {
			return nil, nil, nil, err
		}
		switch conn.Spec.Auth {
		case kwerftv1.GitAuthToken:
			run.auth, run.username, run.credentials = "token", gitUsername(conn.Spec.Provider), builds.CredentialsSecret(conn.Name)
			if err := r.requireKeys(ctx, conn.Name, builds.KeyToken); err != nil {
				return nil, nil, nil, err
			}
		case kwerftv1.GitAuthSSHKey:
			run.auth, run.credentials = "ssh", builds.CredentialsSecret(conn.Name)
			if err := r.requireKeys(ctx, conn.Name, builds.KeySSHPrivateKey, builds.KeyKnownHosts); err != nil {
				return nil, nil, nil, err
			}
		case kwerftv1.GitAuthGitHubApp:
			run.auth, run.username, run.credentials = "token", gitUsername(kwerftv1.GitHub), tokenSecretName(run.jobName)
		}
	}

	var svc corev1.Service
	err := r.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: builds.RegistryService}, &svc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return nil, nil, nil, err
	}
	if ip := svc.Spec.ClusterIP; ip == "" || ip == corev1.ClusterIPNone {
		return nil, nil, &readiness{metav1.ConditionFalse, "RegistryMissing",
			"Waiting for the registry (Service " + builds.RegistryNamespace + "/" + builds.RegistryService + ")"}, nil
	}
	run.registryIP = svc.Spec.ClusterIP
	return run, conn, nil, nil
}

// checkConnection makes sure a project may use the connection for this
// repository, so a token is sent only to its own host and owner.
func checkConnection(conn *kwerftv1.GitConnection, project, repo, transport string) error {
	if p := conn.Spec.Projects; len(p) > 0 && !slices.Contains(p, project) {
		return terminalf("ConnectionNotAllowed", "project %s may not use the Git connection %s", project, conn.Name)
	}
	switch conn.Spec.Auth {
	case kwerftv1.GitAuthSSHKey:
		if transport != "ssh" {
			return terminalf("ConnectionMismatch", "the Git connection %s clones over SSH; use the repository's SSH URL (git@host:owner/repo.git)", conn.Name)
		}
		return nil
	default:
		if transport != "https" {
			return terminalf("ConnectionMismatch", "the Git connection %s clones over HTTPS; use the repository's HTTPS URL", conn.Name)
		}
	}
	if want := repoHost(conn.Spec.URL); repoHost(repo) != want {
		return terminalf("ConnectionMismatch", "the repository is not on %s, the host of Git connection %s", want, conn.Name)
	}
	if o := conn.Spec.Owner; o != "" && !strings.EqualFold(repoOwner(repo), o) {
		return terminalf("ConnectionMismatch", "the Git connection %s is limited to %s", conn.Name, o)
	}
	return nil
}

// requireKeys fails the build when the connection's Secret lacks a key, so
// the user reads "add the token" instead of a clone error.
func (r *BuildReconciler) requireKeys(ctx context.Context, conn string, keys ...string) error {
	var sec corev1.Secret
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(conn)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return terminalf("CredentialsMissing", "the Git connection %s has no credentials yet", conn)
		}
		return err
	}
	for _, k := range keys {
		if len(sec.Data[k]) == 0 {
			return terminalf("CredentialsMissing", "the Git connection %s has no %s", conn, k)
		}
	}
	return nil
}

// ownBy makes the App an owner of the Build (not its controller), so
// deleting the App deletes its builds and, through them, their Jobs.
func (r *BuildReconciler) ownBy(ctx context.Context, b *kwerftv1.Build, app *kwerftv1.App) error {
	if slices.ContainsFunc(b.OwnerReferences, func(o metav1.OwnerReference) bool { return o.UID == app.UID }) {
		return nil
	}
	cp := b.DeepCopy()
	patch := client.MergeFrom(cp.DeepCopy())
	cp.OwnerReferences = append(cp.OwnerReferences, metav1.OwnerReference{
		APIVersion: kwerftv1.GroupVersion.String(), Kind: "App", Name: app.Name, UID: app.UID,
	})
	if err := r.Patch(ctx, cp, patch); err != nil {
		return client.IgnoreNotFound(err)
	}
	b.OwnerReferences = cp.OwnerReferences
	return nil
}

func ownsJob(b *kwerftv1.Build, job *batchv1.Job) bool {
	return job.Namespace == builds.Namespace && job.Labels[builds.LabelBuild] == b.Name && job.Labels[LabelProject] == b.Namespace
}

func jobFinished(job *batchv1.Job) bool {
	return jobCondition(job, batchv1.JobComplete) != nil || jobCondition(job, batchv1.JobFailed) != nil
}

// activeJobs lists the build Jobs that hold a slot, oldest first.
func (r *BuildReconciler) activeJobs(ctx context.Context) ([]batchv1.Job, error) {
	var jobs batchv1.JobList
	if err := r.APIReader.List(ctx, &jobs, client.InNamespace(builds.Namespace), client.HasLabels{builds.LabelBuild}); err != nil {
		return nil, err
	}
	var out []batchv1.Job
	for _, j := range jobs.Items {
		if j.DeletionTimestamp.IsZero() && !jobFinished(&j) {
			out = append(out, j)
		}
	}
	slices.SortFunc(out, func(a, b batchv1.Job) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
	return out, nil
}

// queue lists the builds waiting for a slot, in the order they start.
func (r *BuildReconciler) queue(ctx context.Context) ([]*kwerftv1.Build, error) {
	var list kwerftv1.BuildList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []*kwerftv1.Build
	for i := range list.Items {
		if waiting(&list.Items[i]) {
			out = append(out, &list.Items[i])
		}
	}
	slices.SortStableFunc(out, queueOrder)
	return out, nil
}

// queueOrder is creation order; an App's builds created within the same
// second keep their numbers' order (unnumbered last).
func queueOrder(a, b *kwerftv1.Build) int {
	if c := a.CreationTimestamp.Compare(b.CreationTimestamp.Time); c != 0 {
		return c
	}
	if a.Namespace == b.Namespace && a.Spec.App == b.Spec.App && a.Status.Number != b.Status.Number {
		switch {
		case a.Status.Number == 0:
			return 1
		case b.Status.Number == 0:
			return -1
		case a.Status.Number < b.Status.Number:
			return -1
		}
		return 1
	}
	return olderFirst(a, b)
}

func waiting(b *kwerftv1.Build) bool {
	return b.Status.Job == "" && !buildFinished(b.Status.Phase) && b.DeletionTimestamp.IsZero() &&
		b.Annotations[kwerftv1.AnnotationCancelRequested] == ""
}

func (r *BuildReconciler) describeJob(ctx context.Context, job *batchv1.Job, from *kwerftv1.Build) string {
	return r.describe(ctx, client.ObjectKey{Namespace: job.Labels[LabelProject], Name: job.Labels[builds.LabelBuild]}, from)
}

// describe names another build for a queue message, read live so its
// number (handed out a moment ago) shows.
func (r *BuildReconciler) describe(ctx context.Context, key client.ObjectKey, from *kwerftv1.Build) string {
	var o kwerftv1.Build
	if err := r.APIReader.Get(ctx, key, &o); err != nil {
		return key.Namespace + "/" + key.Name
	}
	return describeBuild(&o, from.Namespace, from.Spec.App)
}

// observe copies the Job's and its pod's state into the Build status.
func (r *BuildReconciler) observe(ctx context.Context, b *kwerftv1.Build) (ctrl.Result, error) {
	st := &b.Status
	job := &batchv1.Job{}
	key := client.ObjectKey{Namespace: builds.Namespace, Name: st.Job}
	err := r.Get(ctx, key, job)
	if apierrors.IsNotFound(err) {
		err = r.APIReader.Get(ctx, key, job) // the cache may lag right after creation
	}
	switch {
	case apierrors.IsNotFound(err):
		st.Phase = kwerftv1.BuildFailed
		st.CompletionTime = ptrTime(r.now())
		st.Message = "The build Job " + st.Job + " was deleted before it finished"
		setReady(&st.Conditions, b.Generation, metav1.ConditionFalse, "JobDeleted", st.Message)
		return ctrl.Result{}, nil
	case err != nil:
		return ctrl.Result{}, err
	case !ownsJob(b, job):
		return ctrl.Result{}, terminalf("JobConflict", "the Job %q does not belong to this build", st.Job)
	}
	if job.Status.StartTime != nil && st.StartTime == nil {
		st.StartTime = job.Status.StartTime
	}

	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(builds.Namespace), client.MatchingLabels{
		builds.LabelBuild: b.Name, LabelProject: b.Namespace,
	}); err != nil {
		return ctrl.Result{}, err
	}
	var pod *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if pod == nil || pod.CreationTimestamp.Before(&p.CreationTimestamp) ||
			(pod.CreationTimestamp.Equal(&p.CreationTimestamp) && pod.Name < p.Name) {
			pod = p
		}
	}
	if pod != nil {
		st.Pod = pod.Name
	}

	if c := jobCondition(job, batchv1.JobComplete); c != nil {
		st.Phase = kwerftv1.BuildSucceeded
		st.CompletionTime = job.Status.CompletionTime
		if st.CompletionTime == nil {
			st.CompletionTime = &c.LastTransitionTime
		}
		if pod != nil {
			if cs := containerStatus(pod, builds.ContainerBuild); cs != nil && cs.State.Terminated != nil {
				st.Digest = parseTermination(cs.State.Terminated.Message).digest
			}
		}
		st.Message = ""
		setReady(&st.Conditions, b.Generation, metav1.ConditionTrue, "Succeeded", "Pushed "+deployImage(b))
		return ctrl.Result{}, nil
	}
	if c := jobCondition(job, batchv1.JobFailed); c != nil {
		st.Phase = kwerftv1.BuildFailed
		st.CompletionTime = &c.LastTransitionTime
		reason := "Failed"
		if c.Reason == batchv1.JobReasonDeadlineExceeded {
			reason = "TimedOut"
			st.Message = "Timed out after " + humanDuration(r.timeout())
		} else {
			st.Message = failureMessage(pod, c)
		}
		setReady(&st.Conditions, b.Generation, metav1.ConditionFalse, reason, st.Message)
		return ctrl.Result{}, nil
	}

	step, waitingMsg := buildStep(pod)
	if step != "" {
		st.Phase = kwerftv1.BuildRunning
		st.Message = step
		setReady(&st.Conditions, b.Generation, metav1.ConditionFalse, "Running", step)
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	st.Phase = kwerftv1.BuildPending
	st.Message = "Waiting for the build pod to start"
	if waitingMsg != "" {
		st.Message = waitingMsg
	}
	setReady(&st.Conditions, b.Generation, metav1.ConditionFalse, "Pending", st.Message)
	return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
}

func containerStatus(pod *corev1.Pod, name string) *corev1.ContainerStatus {
	for _, list := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for i := range list {
			if list[i].Name == name {
				return &list[i]
			}
		}
	}
	return nil
}

var stepNames = map[string]string{
	builds.ContainerClone:   "Cloning",
	builds.ContainerPrepare: "Preparing the Railpack plan",
	builds.ContainerBuild:   "Building",
}

// buildStep says which step of a running build pod is running, or why the
// pod is still waiting.
func buildStep(pod *corev1.Pod) (step, waiting string) {
	if pod == nil {
		return "", ""
	}
	for _, name := range builds.LogContainers {
		cs := containerStatus(pod, name)
		if cs == nil {
			continue
		}
		switch {
		case cs.State.Running != nil:
			return stepNames[name], ""
		case cs.State.Waiting != nil && cs.State.Waiting.Reason != "" &&
			cs.State.Waiting.Reason != "PodInitializing" && cs.State.Waiting.Reason != "ContainerCreating":
			waiting = cs.State.Waiting.Reason
			if cs.State.Waiting.Message != "" {
				waiting += ": " + cs.State.Waiting.Message
			}
			return "", waiting
		}
	}
	return "", ""
}

// failureMessage is the reason a build pod failed: the first container that
// failed, with our own message or the last error line of its log.
func failureMessage(pod *corev1.Pod, c *batchv1.JobCondition) string {
	fallback := c.Message
	if fallback == "" {
		fallback = "The build failed"
	}
	if pod == nil {
		return fallback
	}
	if pod.Status.Reason == "Evicted" {
		return "The build pod was evicted: " + oneLine(pod.Status.Message)
	}
	prefixes := map[string]string{
		builds.ContainerClone:   "Clone failed: ",
		builds.ContainerPrepare: "Railpack: ",
		builds.ContainerBuild:   "",
	}
	for _, name := range builds.LogContainers {
		cs := containerStatus(pod, name)
		if cs == nil || cs.State.Terminated == nil || cs.State.Terminated.ExitCode == 0 {
			continue
		}
		t := cs.State.Terminated
		if t.Reason == "OOMKilled" {
			return stepNames[name] + " ran out of memory"
		}
		if msg := parseTermination(t.Message).message; msg != "" {
			return prefixes[name] + msg
		}
		return fmt.Sprintf("%s%s failed (exit code %d)", prefixes[name], stepNames[name], t.ExitCode)
	}
	return fallback
}

// finish runs once a build is final: its GitHub App token goes at once and
// the App's oldest builds are pruned.
func (r *BuildReconciler) finish(ctx context.Context, b *kwerftv1.Build) error {
	if err := r.deleteTokens(ctx, b.Namespace, b.Name); err != nil {
		return err
	}
	return r.prune(ctx, b.Namespace, b.Spec.App)
}

// deleteTokens removes the build's GitHub App token Secret, if it has one.
func (r *BuildReconciler) deleteTokens(ctx context.Context, namespace, name string) error {
	var sec corev1.Secret
	key := client.ObjectKey{Namespace: builds.Namespace, Name: tokenSecretName(buildJobName(namespace, name))}
	if err := r.APIReader.Get(ctx, key, &sec); err != nil {
		return client.IgnoreNotFound(err)
	}
	if sec.Labels[LabelGitToken] != "true" || sec.Labels[builds.LabelBuild] != name || sec.Labels[LabelProject] != namespace {
		return nil // not ours
	}
	return client.IgnoreNotFound(r.Delete(ctx, &sec))
}

// deleteJobs removes a build's Job (its pods follow) and token Secret.
func (r *BuildReconciler) deleteJobs(ctx context.Context, namespace, name string) error {
	var jobs batchv1.JobList
	if err := r.APIReader.List(ctx, &jobs, client.InNamespace(builds.Namespace),
		client.MatchingLabels{builds.LabelBuild: name, LabelProject: namespace}); err != nil {
		return err
	}
	for i := range jobs.Items {
		if err := r.Delete(ctx, &jobs.Items[i], client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete job: %w", err)
		}
	}
	return r.deleteTokens(ctx, namespace, name)
}

// cleanUpDeleted removes the Job of a Build that is gone; nothing else
// connects the two (they live in different namespaces).
func (r *BuildReconciler) cleanUpDeleted(ctx context.Context, namespace, name string) error {
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &kwerftv1.Build{}); !apierrors.IsNotFound(err) {
		return client.IgnoreNotFound(err) // still there: the cache lagged
	}
	return r.deleteJobs(ctx, namespace, name)
}

// prune keeps the App's newest MaxBuildsPerApp builds. Unfinished builds and
// builds whose image a revision in the App's history runs stay.
func (r *BuildReconciler) prune(ctx context.Context, namespace, appName string) error {
	var list kwerftv1.BuildList
	if err := r.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return err
	}
	var mine []*kwerftv1.Build
	for i := range list.Items {
		if list.Items[i].Spec.App == appName && list.Items[i].DeletionTimestamp.IsZero() {
			mine = append(mine, &list.Items[i])
		}
	}
	if len(mine) <= MaxBuildsPerApp {
		return nil
	}
	slices.SortStableFunc(mine, newestFirst)

	keepNames, keepImages := map[string]bool{}, map[string]bool{}
	var app kwerftv1.App
	switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: appName}, &app); {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		for _, rev := range app.Status.History {
			keepNames[rev.Build] = true
			keepImages[rev.Image] = true
		}
		keepImages[app.Status.Image] = true
		if git := app.Spec.Source.Git; git != nil && git.PinnedImage != "" {
			keepImages[git.PinnedImage] = true
		}
	}
	for _, o := range mine[MaxBuildsPerApp:] {
		if !buildFinished(o.Status.Phase) || keepNames[o.Name] ||
			(o.Status.Image != "" && (keepImages[deployImage(o)] || keepImages[o.Status.Image])) {
			continue
		}
		if err := r.Delete(ctx, o, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("prune build %s: %w", o.Name, err)
		}
	}
	return nil
}

// newestFirst orders by number, unnumbered (just created) builds first.
func newestFirst(a, b *kwerftv1.Build) int {
	an, bn := a.Status.Number, b.Status.Number
	switch {
	case an == bn:
		return -olderFirst(a, b)
	case an == 0:
		return -1
	case bn == 0:
		return 1
	case an > bn:
		return -1
	}
	return 1
}

// buildForJob maps a build Job (or its token Secret) to its Build.
func buildForJob(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if obj.GetNamespace() != builds.Namespace || l[builds.LabelBuild] == "" || l[LabelProject] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: l[LabelProject], Name: l[builds.LabelBuild]}}}
}

// queuedBuilds enqueues every waiting build: a slot may have opened.
func (r *BuildReconciler) queuedBuilds(ctx context.Context, _ client.Object) []reconcile.Request {
	queue, err := r.queue(ctx)
	if err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(queue))
	for _, b := range queue {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(b)})
	}
	return reqs
}

// slotFreed passes the events after which a queued build may start or its
// queue message changes.
var slotFreed = predicate.Funcs{
	CreateFunc:  func(event.CreateEvent) bool { return false },
	GenericFunc: func(event.GenericEvent) bool { return false },
	DeleteFunc:  func(event.DeleteEvent) bool { return true },
	UpdateFunc: func(e event.UpdateEvent) bool {
		o, ok1 := e.ObjectOld.(*kwerftv1.Build)
		n, ok2 := e.ObjectNew.(*kwerftv1.Build)
		// A slot opened; or the queue moved or a build got its number,
		// which the queue messages name.
		return ok1 && ok2 && (!buildFinished(o.Status.Phase) && buildFinished(n.Status.Phase) ||
			o.Status.Job == "" && n.Status.Job != "" || o.Status.Number != n.Status.Number)
	},
}

func (r *BuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	inBuilds := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetNamespace() == builds.Namespace })
	return ctrl.NewControllerManagedBy(mgr).
		// Builds are immutable; status writes do not re-trigger. An
		// annotation carries a cancel request.
		For(&kwerftv1.Build{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&batchv1.Job{}, handler.EnqueueRequestsFromMapFunc(buildForJob), builder.WithPredicates(inBuilds)).
		Watches(&kwerftv1.Build{}, handler.EnqueueRequestsFromMapFunc(r.queuedBuilds), builder.WithPredicates(slotFreed)).
		Named("build").
		Complete(r)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
