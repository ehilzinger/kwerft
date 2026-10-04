package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
)

const (
	testGitToken = "ghp_secret-token-never-in-specs"
	testAppToken = "ghs_installation-token-never-in-specs"
)

// fakeInstallationToken plays the GitHub API for GitHub App connections.
func fakeInstallationToken(_ context.Context, conn *kwerftv1.GitConnection, key []byte) (string, error) {
	if conn.Spec.GitHubApp == nil || len(key) == 0 {
		return "", permanentError{fmt.Errorf("no app")}
	}
	return testAppToken, nil
}

// setupBuildInfra creates what the chart provides for builds: the
// kwerft-builds namespace and the registry Service.
func setupBuildInfra(ctx context.Context) error {
	if err := k8s.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: builds.Namespace}}); err != nil {
		return err
	}
	return k8s.Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: builds.RegistryNamespace, Name: builds.RegistryService},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "zot"},
			Ports:    []corev1.ServicePort{{Port: builds.RegistryPort, TargetPort: intstr.FromInt32(5000)}},
		},
	})
}

// sha is a commit whose short form (the image tag) differs per n.
func sha(n int) string { return fmt.Sprintf("%012x%028x", n, n) }

func gitAppSpec(repo, connection string) kwerftv1.AppSpec {
	return kwerftv1.AppSpec{Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: repo, Connection: connection}}}
}

// gitProject creates a project with a Git app and cancels its unfinished
// builds when the test ends: the build slot is shared by the whole suite.
func gitProject(t *testing.T, ns, app string, spec kwerftv1.AppSpec) *kwerftv1.App {
	t.Helper()
	projectNamespace(t, ns)
	a := createApp(t, ns, app, spec)
	t.Cleanup(func() { cancelAllBuilds(t, ns) })
	waitForApp(t, a, "AwaitingBuild")
	return a
}

func cancelAllBuilds(t *testing.T, ns string) {
	ctx := context.Background()
	var list kwerftv1.BuildList
	if err := k8s.List(ctx, &list, client.InNamespace(ns)); err != nil {
		t.Error(err)
		return
	}
	for i := range list.Items {
		b := &list.Items[i]
		if buildFinished(b.Status.Phase) {
			continue
		}
		requestCancel(t, b, "cleanup")
		waitForBuild(t, b, buildPhase(kwerftv1.BuildCancelled, kwerftv1.BuildFailed, kwerftv1.BuildSucceeded))
	}
}

func newBuild(t *testing.T, app *kwerftv1.App, commit int, trigger string, deploy bool, annotations map[string]string) *kwerftv1.Build {
	t.Helper()
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	b, err := builds.New(app, builds.Request{
		Commit:  builds.Commit{SHA: sha(commit), Branch: "main", Message: "Commit " + fmt.Sprint(commit), Author: "Ada"},
		Trigger: trigger, RequestedBy: "ada@example.com", Deploy: deploy,
	})
	if err != nil {
		t.Fatal(err)
	}
	b.Annotations = annotations
	if err := k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func waitForBuild(t *testing.T, b *kwerftv1.Build, check func(*kwerftv1.Build) error) *kwerftv1.Build {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(b), b); err != nil {
			return err
		}
		return check(b)
	})
	return b
}

func buildPhase(want ...kwerftv1.BuildPhase) func(*kwerftv1.Build) error {
	return func(b *kwerftv1.Build) error {
		if !slices.Contains(want, b.Status.Phase) {
			return fmt.Errorf("phase %q, want %v (message %q)", b.Status.Phase, want, b.Status.Message)
		}
		return nil
	}
}

func buildStarted(b *kwerftv1.Build) error {
	if b.Status.Job == "" {
		return fmt.Errorf("no job yet (phase %q, message %q)", b.Status.Phase, b.Status.Message)
	}
	return nil
}

func requestCancel(t *testing.T, b *kwerftv1.Build, by string) {
	t.Helper()
	patch := client.MergeFrom(b.DeepCopy())
	if b.Annotations == nil {
		b.Annotations = map[string]string{}
	}
	b.Annotations[kwerftv1.AnnotationCancelRequested] = by
	if err := k8s.Patch(context.Background(), b, patch); client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
}

// createBuildPod plays the Job controller and kubelet: the build's pod, with
// the build container in the given state.
func createBuildPod(t *testing.T, b *kwerftv1.Build, build corev1.ContainerState, phase corev1.PodPhase) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: b.Status.Job + "-pod",
			Labels: map[string]string{builds.LabelBuild: b.Name, LabelProject: b.Namespace}},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{{Name: builds.ContainerClone, Image: DefaultBuildKitImage}},
			Containers:     []corev1.Container{{Name: builds.ContainerBuild, Image: DefaultBuildKitImage}},
		},
	}
	if err := k8s.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{
		Phase: phase,
		InitContainerStatuses: []corev1.ContainerStatus{{Name: builds.ContainerClone, Image: DefaultBuildKitImage,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: builds.ContainerBuild, Image: DefaultBuildKitImage, State: build}},
	}
	if err := k8s.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

// endBuildJob plays the Job controller: complete, or failed with reason.
func endBuildJob(t *testing.T, b *kwerftv1.Build, failReason string) {
	t.Helper()
	job := waitForJob(t, builds.Namespace, b.Status.Job)
	now := metav1.Now()
	start := metav1.NewTime(now.Add(-time.Minute))
	job.Status.StartTime = &start
	if failReason == "" {
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, Reason: "CompletionsReached", LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, Reason: "CompletionsReached", LastTransitionTime: now},
		}
	} else {
		job.Status.Failed = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: failReason, Message: "failed", LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: failReason, Message: "failed", LastTransitionTime: now},
		}
	}
	if err := k8s.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

const testDigest = "sha256:4f2c1ab9e01d7c4f2c1ab9e01d7c4f2c1ab9e01d7c4f2c1ab9e01d7c4f2c1a"

// succeed runs a started build to success with testDigest.
func succeed(t *testing.T, b *kwerftv1.Build) *kwerftv1.Build {
	t.Helper()
	waitForBuild(t, b, buildStarted)
	createBuildPod(t, b, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 0, Reason: "Completed", Message: "digest=" + testDigest}}, corev1.PodSucceeded)
	endBuildJob(t, b, "")
	return waitForBuild(t, b, buildPhase(kwerftv1.BuildSucceeded))
}

func createGitConnection(t *testing.T, name string, spec kwerftv1.GitConnectionSpec, secret map[string]string) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Create(ctx, &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}); err != nil {
		t.Fatal(err)
	}
	if secret != nil {
		if err := k8s.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)},
			StringData: secret,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func deploymentImage(t *testing.T, ns, name string) string {
	t.Helper()
	var d appsv1.Deployment
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &d); err != nil {
		return ""
	}
	return d.Spec.Template.Spec.Containers[0].Image
}

func TestBuildRendersJobAndDeploys(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createGitConnection(t, "acme-token", kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.GitHub, URL: "https://github.com", Auth: kwerftv1.GitAuthToken, Owner: "acme",
	}, map[string]string{builds.KeyToken: testGitToken, builds.KeyWebhookSecret: "whsec"})
	app := gitProject(t, "bshop", "api", gitAppSpec("https://github.com/acme/api.git", "acme-token"))

	b := newBuild(t, app, 1, "push", true, nil)
	b = waitForBuild(t, b, buildStarted)
	if b.Status.Number != 1 || b.Status.Image != builds.ImageRef("bshop", "api", sha(1)) {
		t.Errorf("status = %+v", b.Status)
	}
	if !slices.ContainsFunc(b.OwnerReferences, func(o metav1.OwnerReference) bool { return o.Kind == "App" && o.Name == "api" }) {
		t.Errorf("owner references = %+v", b.OwnerReferences)
	}

	job := waitForJob(t, builds.Namespace, b.Status.Job)
	if job.Name != "bshop-"+b.Name || job.Labels[builds.LabelBuild] != b.Name || job.Labels[LabelProject] != "bshop" {
		t.Errorf("job %s labels = %v", job.Name, job.Labels)
	}
	s := job.Spec
	if *s.BackoffLimit != 0 || *s.ActiveDeadlineSeconds != 1800 || *s.TTLSecondsAfterFinished != buildLogTTL {
		t.Errorf("backoff %d deadline %d ttl %d", *s.BackoffLimit, *s.ActiveDeadlineSeconds, *s.TTLSecondsAfterFinished)
	}
	pod := s.Template.Spec
	if l := s.Template.Labels; l[builds.LabelBuild] != b.Name || l[LabelProject] != "bshop" {
		t.Errorf("pod labels = %v", l)
	}
	if pod.PriorityClassName != BatchPriorityClass || *pod.AutomountServiceAccountToken || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("pod spec = %+v", pod)
	}
	if sc := pod.SecurityContext; !*sc.RunAsNonRoot || *sc.RunAsUser != 1000 || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod security = %+v", sc)
	}
	var svc corev1.Service
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.RegistryNamespace, Name: builds.RegistryService}, &svc); err != nil {
		t.Fatal(err)
	}
	if h := pod.HostAliases; len(h) != 1 || h[0].IP != svc.Spec.ClusterIP || h[0].Hostnames[0] != "registry.kwerft.internal" {
		t.Errorf("host aliases = %+v", h)
	}
	if len(pod.InitContainers) != 1 || pod.InitContainers[0].Name != builds.ContainerClone || len(pod.Containers) != 1 {
		t.Fatalf("containers = %+v / %+v", pod.InitContainers, pod.Containers)
	}
	clone, build := pod.InitContainers[0], pod.Containers[0]
	if *clone.SecurityContext.AllowPrivilegeEscalation || !*clone.SecurityContext.ReadOnlyRootFilesystem {
		t.Errorf("clone security = %+v", clone.SecurityContext)
	}
	if sc := build.SecurityContext; sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined ||
		sc.AppArmorProfile.Type != corev1.AppArmorProfileTypeUnconfined || sc.Privileged != nil {
		t.Errorf("build security = %+v", sc)
	}
	if l := build.Resources.Limits; !l.Cpu().Equal(resource.MustParse("2")) || !l.Memory().Equal(resource.MustParse("3Gi")) {
		t.Errorf("build limits = %v", l)
	}
	envOf := func(c corev1.Container) map[string]string {
		m := map[string]string{}
		for _, e := range c.Env {
			m[e.Name] = e.Value
		}
		return m
	}
	if e := envOf(build); e["KWERFT_IMAGE"] != builds.ImageRef("bshop", "api", sha(1)) || e["KWERFT_CACHE"] != builds.CacheRef("bshop", "api") ||
		e["KWERFT_BUILDER"] != "dockerfile" || e["KWERFT_DOCKERFILE"] != "Dockerfile" || e["KWERFT_CONTEXT"] != "" {
		t.Errorf("build env = %v", e)
	}
	if e := envOf(clone); e["KWERFT_GIT_AUTH"] != "token" || e["KWERFT_GIT_USERNAME"] != "x-access-token" || e["KWERFT_COMMIT"] != sha(1) {
		t.Errorf("clone env = %v", e)
	}
	// Credentials: only the token, only through the Secret, only in the clone.
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), testGitToken) || strings.Contains(string(raw), "whsec") {
		t.Error("a credential appears in the Job")
	}
	var creds *corev1.Volume
	for i, v := range pod.Volumes {
		if v.Name == "credentials" {
			creds = &pod.Volumes[i]
		}
	}
	if creds == nil || creds.Secret.SecretName != "git-acme-token" || len(creds.Secret.Items) != 1 || creds.Secret.Items[0].Key != builds.KeyToken {
		t.Errorf("credentials volume = %+v", creds)
	}
	for _, m := range build.VolumeMounts {
		if m.Name == "credentials" {
			t.Error("the build container mounts the credentials")
		}
	}

	// Running, then success: the App rolls out the digest-pinned image.
	createBuildPod(t, b, corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}, corev1.PodRunning)
	b = waitForBuild(t, b, buildPhase(kwerftv1.BuildRunning))
	if b.Status.Pod != b.Status.Job+"-pod" || b.Status.Message != "Building" {
		t.Errorf("running status = %+v", b.Status)
	}
	var p corev1.Pod
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: b.Status.Pod}, &p); err != nil {
		t.Fatal(err)
	}
	p.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 0, Reason: "Completed", Message: "digest=" + testDigest}}
	if err := k8s.Status().Update(ctx, &p); err != nil {
		t.Fatal(err)
	}
	endBuildJob(t, b, "")
	b = waitForBuild(t, b, buildPhase(kwerftv1.BuildSucceeded))
	if b.Status.Digest != testDigest || b.Status.CompletionTime == nil || b.Status.StartTime == nil {
		t.Errorf("succeeded status = %+v", b.Status)
	}
	want := builds.ImageRef("bshop", "api", sha(1)) + "@" + testDigest
	eventually(t, func() error {
		if got := deploymentImage(t, "bshop", "api"); got != want {
			return fmt.Errorf("deployment image %q, want %q", got, want)
		}
		return nil
	})
	app = waitForApp(t, app, "Progressing")
	if h := app.Status.History; len(h) != 1 || h[0].Build != b.Name || h[0].Commit != sha(1) || h[0].Image != want {
		t.Errorf("history = %+v", h)
	}
}

func TestBuildNumbersQueueAndCancel(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	app := gitProject(t, "bqueue", "web", gitAppSpec("https://github.com/acme/web.git", ""))

	b1 := newBuild(t, app, 1, "push", true, nil)
	b1 = waitForBuild(t, b1, buildStarted)
	b2 := newBuild(t, app, 2, "push", true, nil)
	b3 := newBuild(t, app, 3, "manual", true, nil)

	// One slot: the others queue, in order, and say what they wait for.
	// (Builds created within the same second are ordered by name.)
	queuedBehind := func(b *kwerftv1.Build) error {
		want := fmt.Sprintf("Queued behind #%d", b.Status.Number-1)
		if b.Status.Number < 2 || b.Status.Message != want || b.Status.Job != "" {
			return fmt.Errorf("number %d message %q job %q", b.Status.Number, b.Status.Message, b.Status.Job)
		}
		return nil
	}
	b2 = waitForBuild(t, b2, queuedBehind)
	b3 = waitForBuild(t, b3, queuedBehind)
	numbers := []int64{b1.Status.Number, b2.Status.Number, b3.Status.Number}
	slices.Sort(numbers)
	if !slices.Equal(numbers, []int64{1, 2, 3}) || b1.Status.Number != 1 {
		t.Errorf("numbers = %v (first %d)", numbers, b1.Status.Number)
	}

	// Cancel a queued build: it never gets a Job.
	last := b3
	if b2.Status.Number == 3 {
		last = b2
	}
	requestCancel(t, last, "ada@example.com")
	last = waitForBuild(t, last, buildPhase(kwerftv1.BuildCancelled))
	if last.Status.Job != "" || last.Status.Message != "Cancelled by ada@example.com" || last.Status.CompletionTime == nil {
		t.Errorf("cancelled queued build = %+v", last.Status)
	}

	// The first finishes; the next one starts.
	succeed(t, b1)
	next := b2
	if last == b2 {
		next = b3
	}
	next = waitForBuild(t, next, buildStarted)

	// Cancel a running build: its Job goes.
	requestCancel(t, next, "bob@example.com")
	next = waitForBuild(t, next, buildPhase(kwerftv1.BuildCancelled))
	eventually(t, func() error {
		err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: next.Status.Job}, &batchv1.Job{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("job still there (err=%v)", err)
		}
		return nil
	})
	if reason, _ := readyReason(next.Status.Conditions, next.Generation); reason != "Cancelled" {
		t.Errorf("reason = %q", reason)
	}
}

func TestPullRequestBuildDoesNotDeployAndPinnedImageWins(t *testing.T) {
	requireEnvtest(t)
	app := gitProject(t, "bpr", "api", gitAppSpec("https://github.com/acme/api.git", ""))

	main := succeed(t, newBuild(t, app, 1, "push", true, nil))
	mainImage := deployImage(main)
	eventually(t, func() error {
		if got := deploymentImage(t, "bpr", "api"); got != mainImage {
			return fmt.Errorf("image %q, want %q", got, mainImage)
		}
		return nil
	})

	pr := newBuild(t, app, 2, "pull-request", true, nil)
	if pr.Spec.Deploy {
		t.Fatal("builds.New let a pull-request build deploy")
	}
	succeed(t, pr)
	poke(t, app) // make sure the App reconciled after the PR build
	app = waitForApp(t, app, "Progressing")
	if got := deploymentImage(t, "bpr", "api"); got != mainImage || app.Status.Revision != 1 {
		t.Errorf("after a PR build: image %q revision %d", got, app.Status.Revision)
	}

	// A rollback pins an image; it wins over newer builds.
	newer := succeed(t, newBuild(t, app, 3, "push", true, nil))
	eventually(t, func() error {
		if got := deploymentImage(t, "bpr", "api"); got != deployImage(newer) {
			return fmt.Errorf("image %q, want the newer build", got)
		}
		return nil
	})
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	patch := client.MergeFrom(app.DeepCopy())
	app.Spec.Source.Git.PinnedImage = mainImage
	if err := k8s.Patch(context.Background(), app, patch); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if got := deploymentImage(t, "bpr", "api"); got != mainImage {
			return fmt.Errorf("image %q, want the pinned one", got)
		}
		return nil
	})
	app = waitForApp(t, app, "Progressing")
	if h := app.Status.History[0]; h.Image != mainImage || h.Build != main.Name || h.Commit != sha(1) {
		t.Errorf("pinned revision = %+v", h)
	}
}

func TestBuildTimeoutAndFailureMessages(t *testing.T) {
	requireEnvtest(t)
	app := gitProject(t, "bfail", "api", gitAppSpec("https://github.com/acme/api.git", ""))

	slow := waitForBuild(t, newBuild(t, app, 1, "push", true, nil), buildStarted)
	endBuildJob(t, slow, batchv1.JobReasonDeadlineExceeded)
	slow = waitForBuild(t, slow, buildPhase(kwerftv1.BuildFailed))
	if slow.Status.Message != "Timed out after 30m" {
		t.Errorf("message = %q", slow.Status.Message)
	}
	if reason, _ := readyReason(slow.Status.Conditions, slow.Generation); reason != "TimedOut" {
		t.Errorf("reason = %q", reason)
	}

	broken := waitForBuild(t, newBuild(t, app, 2, "push", true, nil), buildStarted)
	createBuildPod(t, broken, corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
		ExitCode: 1, Reason: "Error", Message: "error=Dockerfile not found: Dockerfile"}}, corev1.PodFailed)
	endBuildJob(t, broken, "BackoffLimitExceeded")
	broken = waitForBuild(t, broken, buildPhase(kwerftv1.BuildFailed))
	if broken.Status.Message != "Dockerfile not found: Dockerfile" {
		t.Errorf("message = %q", broken.Status.Message)
	}
	waitForApp(t, app, "AwaitingBuild")
}

func TestBuildRefusesForeignConnections(t *testing.T) {
	requireEnvtest(t)
	createGitConnection(t, "other-team", kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.GitLab, URL: "https://gitlab.example.com", Auth: kwerftv1.GitAuthToken, Projects: []string{"someone-else"},
	}, map[string]string{builds.KeyToken: testGitToken})
	createGitConnection(t, "gl", kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.GitLab, URL: "https://gitlab.example.com", Auth: kwerftv1.GitAuthToken,
	}, map[string]string{builds.KeyToken: testGitToken})

	app := gitProject(t, "bconn", "api", gitAppSpec("https://gitlab.example.com/acme/api.git", "other-team"))
	b := waitForBuild(t, newBuild(t, app, 1, "push", true, nil), buildPhase(kwerftv1.BuildFailed))
	if reason, _ := readyReason(b.Status.Conditions, b.Generation); reason != "ConnectionNotAllowed" || b.Status.Job != "" {
		t.Errorf("reason %q job %q message %q", reason, b.Status.Job, b.Status.Message)
	}

	// A token goes only to its own host.
	other := createApp(t, "bconn", "elsewhere", gitAppSpec("https://evil.example.net/acme/api.git", "gl"))
	b = waitForBuild(t, newBuild(t, other, 2, "manual", true, nil), buildPhase(kwerftv1.BuildFailed))
	if reason, _ := readyReason(b.Status.Conditions, b.Generation); reason != "ConnectionMismatch" {
		t.Errorf("reason %q message %q", reason, b.Status.Message)
	}
}

func TestBuildWithGitHubAppToken(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createGitConnection(t, "acme-app", kwerftv1.GitConnectionSpec{
		Provider: kwerftv1.GitHub, URL: "https://github.com", Auth: kwerftv1.GitAuthGitHubApp,
		GitHubApp: &kwerftv1.GitHubAppSettings{AppID: 1, InstallationID: 2},
	}, map[string]string{builds.KeyGitHubAppKey: "-----BEGIN RSA PRIVATE KEY-----\nfake\n-----END RSA PRIVATE KEY-----\n"})
	app := gitProject(t, "bghapp", "api", gitAppSpec("https://github.com/acme/api.git", "acme-app"))

	b := waitForBuild(t, newBuild(t, app, 1, "push", true, nil), buildStarted)
	job := waitForJob(t, builds.Namespace, b.Status.Job)
	raw, _ := json.Marshal(job)
	if strings.Contains(string(raw), testAppToken) || strings.Contains(string(raw), "PRIVATE KEY") {
		t.Error("a credential appears in the Job")
	}
	var sec corev1.Secret
	key := client.ObjectKey{Namespace: builds.Namespace, Name: tokenSecretName(job.Name)}
	eventually(t, func() error { return k8s.Get(ctx, key, &sec) })
	if string(sec.Data[builds.KeyToken]) != testAppToken || len(sec.Data) != 1 {
		t.Errorf("token secret data keys = %d", len(sec.Data))
	}
	if o := metav1.GetControllerOf(&sec); o == nil || o.Kind != "Job" || o.UID != job.UID {
		t.Errorf("token secret owner = %+v", o)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == "credentials" && v.Secret.SecretName != key.Name {
			t.Errorf("clone mounts %s", v.Secret.SecretName)
		}
	}

	succeed(t, b)
	eventually(t, func() error {
		if err := k8s.Get(ctx, key, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("token secret still there (err=%v)", err)
		}
		return nil
	})
}

func TestBuildRetentionKeepsRunningRevision(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	app := gitProject(t, "bkeep", "api", gitAppSpec("https://github.com/acme/api.git", ""))
	first := succeed(t, newBuild(t, app, 1, "push", true, nil))
	eventually(t, func() error {
		if got := deploymentImage(t, "bkeep", "api"); got != deployImage(first) {
			return fmt.Errorf("image %q", got)
		}
		return nil
	})

	// 21 more builds, cancelled at once: 22 in all, two over the limit.
	cancelled := map[string]string{kwerftv1.AnnotationCancelRequested: "test"}
	var made []*kwerftv1.Build
	for i := 2; i <= MaxBuildsPerApp+2; i++ {
		made = append(made, newBuild(t, app, i, "push", true, cancelled))
	}
	for _, b := range made {
		waitForBuild(t, b, buildPhase(kwerftv1.BuildCancelled))
	}
	eventually(t, func() error {
		poke(t, first) // a finished build's reconcile prunes; #1 stays
		var list kwerftv1.BuildList
		if err := k8s.List(ctx, &list, client.InNamespace("bkeep")); err != nil {
			return err
		}
		var nums []int64
		for _, b := range list.Items {
			nums = append(nums, b.Status.Number)
		}
		slices.Sort(nums)
		// #1 is the running revision's build and stays; #2, the oldest
		// other one, goes.
		if len(nums) != MaxBuildsPerApp+1 || nums[0] != 1 || nums[1] != 3 {
			return fmt.Errorf("numbers = %v", nums)
		}
		return nil
	})
}

func TestTaskFromGitAppRunsBuiltImage(t *testing.T) {
	requireEnvtest(t)
	app := gitProject(t, "btask", "api", gitAppSpec("https://github.com/acme/api.git", ""))
	task := createTask(t, "btask", "migrate", kwerftv1.TaskSpec{FromApp: "api", Command: []string{"./migrate"}})
	waitForTask(t, task, taskReason("AwaitingBuild"))

	b := succeed(t, newBuild(t, app, 1, "push", true, nil))
	job := waitForJob(t, "btask", "migrate")
	if got := job.Spec.Template.Spec.Containers[0].Image; got != deployImage(b) {
		t.Errorf("task image = %q, want %q", got, deployImage(b))
	}
}
