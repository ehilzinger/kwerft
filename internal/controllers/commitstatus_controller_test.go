// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/git/gittest"
)

// statusFixture is a connection with stored credentials on a fake host
// and a reporter for it.
func statusFixture(t *testing.T, name string, spec kwerftv1.GitConnectionSpec, creds map[string]string) (*gittest.Server, *CommitStatusReconciler) {
	t.Helper()
	ctx := context.Background()
	ensureNamespace(t, builds.Namespace)
	ensureNamespace(t, "gitstatus")
	fake := newGitFake(t)
	spec.URL = fake.URL
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := k8s.Create(ctx, gc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc) })
	gitReconcile(t, &GitConnectionReconciler{Client: k8s, Git: fake.Factory(), ConsoleDomain: testConsoleDomain}, name)
	for k, v := range creds {
		if v == "$token" {
			v = fake.Token
		} else if v == "$appkey" {
			v = string(fake.AppKeyPEM)
		}
		setCredential(t, name, k, v)
	}
	return fake, &CommitStatusReconciler{Client: k8s, Git: fake.Factory(), ConsoleDomain: "ops.example.com"}
}

func newTestBuild(t *testing.T, fake *gittest.Server, connection, app string) *kwerftv1.Build {
	t.Helper()
	b := &kwerftv1.Build{ObjectMeta: metav1.ObjectMeta{Namespace: "gitstatus", GenerateName: app + "-"}, Spec: kwerftv1.BuildSpec{
		App: app, Commit: gitSHA, Branch: "main", Trigger: "push", Deploy: true,
		Source: kwerftv1.BuildSource{Repository: fake.URL + "/acme/api.git", Builder: "dockerfile", Connection: connection},
	}}
	if err := k8s.Create(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), b) })
	return b
}

func report(t *testing.T, r *CommitStatusReconciler, b *kwerftv1.Build) error {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Name}})
	return err
}

func setPhase(t *testing.T, b *kwerftv1.Build, phase kwerftv1.BuildPhase) {
	t.Helper()
	ctx := context.Background()
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(b), b); err != nil {
		t.Fatal(err)
	}
	b.Status.Phase = phase
	b.Status.Number = 4
	if phase == kwerftv1.BuildSucceeded || phase == kwerftv1.BuildFailed {
		start := metav1.NewTime(time.Now().Add(-92 * time.Second))
		done := metav1.Now()
		b.Status.StartTime, b.Status.CompletionTime = &start, &done
	}
	if err := k8s.Status().Update(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func annotations(t *testing.T, b *kwerftv1.Build) map[string]string {
	t.Helper()
	var got kwerftv1.Build
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(b), &got); err != nil {
		t.Fatal(err)
	}
	return got.Annotations
}

func TestCommitStatusReportsEachPhaseOnce(t *testing.T) {
	requireEnvtest(t)
	fake, r := statusFixture(t, "status-gitea", kwerftv1.GitConnectionSpec{Provider: kwerftv1.Gitea, Auth: kwerftv1.GitAuthToken},
		map[string]string{builds.KeyToken: "$token"})
	b := newTestBuild(t, fake, "status-gitea", "api")

	// Queued, reported once however often it reconciles.
	for range 3 {
		if err := report(t, r, b); err != nil {
			t.Fatal(err)
		}
	}
	if a := annotations(t, b); a[AnnotationReportedPhase] != "Pending" {
		t.Errorf("annotations = %v", a)
	}
	setPhase(t, b, kwerftv1.BuildRunning)
	_ = report(t, r, b)
	_ = report(t, r, b)

	// The host is down: the report fails, is retried, and nothing is marked.
	setPhase(t, b, kwerftv1.BuildSucceeded)
	fake.Fail(http.StatusBadGateway)
	if err := report(t, r, b); err == nil {
		t.Error("a failed report returns no error (no retry)")
	}
	if a := annotations(t, b); a[AnnotationReportedPhase] != "Running" {
		t.Errorf("marked although the report failed: %v", a)
	}
	fake.Fail(0)
	if err := report(t, r, b); err != nil {
		t.Fatal(err)
	}

	var states []string
	for _, s := range fake.Statuses() {
		states = append(states, s.State)
		if s.Context != "kwerft/gitstatus/api" || s.SHA != gitSHA || s.TargetURL != "https://ops.example.com/apps/gitstatus/api?build="+b.Name {
			t.Errorf("status = %+v", s)
		}
	}
	if !slices.Equal(states, []string{"pending", "pending", "success"}) {
		t.Errorf("states = %v", states)
	}
	if last := fake.Statuses()[2]; last.Description != "Build #4 succeeded in 1m32s; deploying" {
		t.Errorf("description = %q", last.Description)
	}

	// A host that refuses for good (no permission) is not retried forever.
	b2 := newTestBuild(t, fake, "status-gitea", "api")
	fake.Fail(http.StatusForbidden)
	if err := report(t, r, b2); err != nil {
		t.Errorf("a permanent refusal is retried: %v", err)
	}
	if a := annotations(t, b2); a[AnnotationReportedPhase] != "Pending" {
		t.Errorf("annotations = %v", a)
	}
	fake.Fail(0)
}

func TestCommitStatusGitHubAppUpdatesOneCheckRun(t *testing.T) {
	requireEnvtest(t)
	fake, r := statusFixture(t, "status-app", kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, Auth: kwerftv1.GitAuthGitHubApp,
		GitHubApp: &kwerftv1.GitHubAppSettings{AppID: 4242, InstallationID: 777}}, map[string]string{builds.KeyGitHubAppKey: "$appkey"})
	b := newTestBuild(t, fake, "status-app", "api")
	if err := report(t, r, b); err != nil {
		t.Fatal(err)
	}
	run := annotations(t, b)[AnnotationCheckRun]
	if run == "" {
		t.Fatal("no check run recorded")
	}
	setPhase(t, b, kwerftv1.BuildFailed)
	if err := report(t, r, b); err != nil {
		t.Fatal(err)
	}
	st := fake.Statuses()
	if len(st) != 2 || st[0].CheckRun != st[1].CheckRun || st[0].State != "queued" || st[1].State != "completed" || st[1].Conclusion != "failure" {
		t.Errorf("check runs = %+v", st)
	}
	if annotations(t, b)[AnnotationCheckRun] != run {
		t.Error("the check run changed")
	}
}

func TestCommitStatusSkipsConnectionsWithoutChecks(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	// A public connection reports nothing; nor does one whose projects
	// exclude the build's.
	fake, r := statusFixture(t, "status-public", kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, Auth: kwerftv1.GitAuthNone}, nil)
	b := newTestBuild(t, fake, "status-public", "api")
	if err := report(t, r, b); err != nil {
		t.Fatal(err)
	}
	fake2, r2 := statusFixture(t, "status-other", kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitLab, Auth: kwerftv1.GitAuthToken,
		Projects: []string{"elsewhere"}}, map[string]string{builds.KeyToken: "$token"})
	b2 := newTestBuild(t, fake2, "status-other", "api")
	if err := report(t, r2, b2); err != nil {
		t.Fatal(err)
	}
	if len(fake.Statuses())+len(fake2.Statuses()) != 0 {
		t.Errorf("reported: %+v %+v", fake.Statuses(), fake2.Statuses())
	}
	// A build without a connection (public repository) neither.
	b3 := newTestBuild(t, fake, "", "api")
	if err := report(t, r, b3); err != nil {
		t.Fatal(err)
	}
	var got kwerftv1.Build
	_ = k8s.Get(ctx, client.ObjectKeyFromObject(b3), &got)
	if got.Annotations[AnnotationReportedPhase] != "" {
		t.Errorf("annotations = %v", got.Annotations)
	}
}
