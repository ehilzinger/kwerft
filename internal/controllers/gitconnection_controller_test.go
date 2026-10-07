// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/git/gittest"
)

// The Git reconcilers against the test API server and a fake Git host. The
// tests call Reconcile themselves, so every step is deterministic; the
// manager in TestMain does not run them.

const gitSHA = "4f2c1ab9d0e5c3b2a1908f7e6d5c4b3a29180716"

func ensureNamespace(t *testing.T, name string) {
	t.Helper()
	err := k8s.Create(context.Background(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func newGitFake(t *testing.T) *gittest.Server {
	fake := gittest.New(t)
	fake.AddRepo("acme/api", "main", gittest.Commit{SHA: gitSHA, Message: "Fix checkout", Author: "Mara"})
	fake.AddRepo("acme/web", "main", gittest.Commit{SHA: gitSHA, Message: "Fix checkout", Author: "Mara"})
	return fake
}

func gitReconcile(t *testing.T, r *GitConnectionReconciler, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	if err != nil {
		t.Fatalf("reconcile %s: %v", name, err)
	}
	return res
}

func getConnection(t *testing.T, name string) *kwerftv1.GitConnection {
	t.Helper()
	var gc kwerftv1.GitConnection
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &gc); err != nil {
		t.Fatal(err)
	}
	return &gc
}

func credentials(t *testing.T, name string) *corev1.Secret {
	t.Helper()
	var sec corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)}, &sec); err != nil {
		t.Fatal(err)
	}
	return &sec
}

func setCredential(t *testing.T, name, key, value string) {
	t.Helper()
	sec := credentials(t, name)
	sec.Data[key] = []byte(value)
	if err := k8s.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
}

func ready(t *testing.T, gc *kwerftv1.GitConnection) *metav1.Condition {
	t.Helper()
	c := meta.FindStatusCondition(gc.Status.Conditions, ConditionReady)
	if c == nil {
		t.Fatalf("no Ready condition: %+v", gc.Status)
	}
	return c
}

func gitApp(t *testing.T, ns, name, repository, connection string) {
	t.Helper()
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Git: &kwerftv1.GitSource{Repository: repository, Branch: "main", Connection: connection}},
	}}
	if err := k8s.Create(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), app) })
}

func credentialsRole(t *testing.T) *rbacv1.Role {
	t.Helper()
	var role rbacv1.Role
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: builds.Namespace, Name: GitCredentialsRole}, &role); err != nil {
		t.Fatal(err)
	}
	return &role
}

func TestGitConnectionTokenLifecycle(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ensureNamespace(t, builds.Namespace)
	ensureNamespace(t, "gitconn-a")
	fake := newGitFake(t)
	clock := time.Now()
	r := &GitConnectionReconciler{Client: k8s, Git: fake.Factory(), ConsoleDomain: testConsoleDomain, Now: func() time.Time { return clock }}

	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "gh-token"},
		Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, URL: fake.URL, Auth: kwerftv1.GitAuthToken}}
	if err := k8s.Create(ctx, gc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc) })

	// First pass: the Secret with a webhook secret, owned by the connection,
	// and patch rights on exactly it. The token is not there yet.
	if res := gitReconcile(t, r, gc.Name); res.RequeueAfter == 0 || res.RequeueAfter > 5*time.Second {
		t.Errorf("waiting for credentials requeues soon, got %v", res.RequeueAfter)
	}
	sec := credentials(t, gc.Name)
	secret := string(sec.Data[builds.KeyWebhookSecret])
	if len(secret) < 40 || !metav1.IsControlledBy(sec, getConnection(t, gc.Name)) || sec.Labels[LabelGitConnection] != gc.Name {
		t.Errorf("secret = %+v", sec.ObjectMeta)
	}
	role := credentialsRole(t)
	if len(role.Rules) != 1 || !slices.Contains(role.Rules[0].ResourceNames, "git-gh-token") || !slices.Equal(role.Rules[0].Verbs, []string{"patch"}) ||
		!slices.Equal(role.Rules[0].Resources, []string{"secrets"}) {
		t.Errorf("role = %+v", role.Rules)
	}
	var rb rbacv1.RoleBinding
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: GitCredentialsRole}, &rb); err != nil || len(rb.Subjects) != 2 {
		t.Errorf("binding = %+v %v", rb.Subjects, err)
	}
	if c := ready(t, getConnection(t, gc.Name)); c.Status != metav1.ConditionUnknown || c.Reason != "WaitingForCredentials" {
		t.Errorf("before the token: %+v", c)
	}
	// Still missing a minute later: an error the user sees.
	clock = clock.Add(2 * time.Minute)
	gitReconcile(t, r, gc.Name)
	if c := ready(t, getConnection(t, gc.Name)); c.Status != metav1.ConditionFalse || c.Reason != "CredentialsMissing" {
		t.Errorf("no token: %+v", c)
	}

	// With the token: verified, with the webhook URL, nothing to hook yet.
	setCredential(t, gc.Name, builds.KeyToken, fake.Token)
	gitReconcile(t, r, gc.Name)
	got := getConnection(t, gc.Name)
	if c := ready(t, got); c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "builder") {
		t.Errorf("ready = %+v", c)
	}
	if got.Status.Account != "builder" || !strings.HasSuffix(got.Status.WebhookURL, "/api/v1/hooks/git/gh-token") ||
		!strings.HasPrefix(got.Status.WebhookURL, "https://") || !got.Status.WebhookAutomatic || len(got.Status.Webhooks) != 0 {
		t.Errorf("status = %+v", got.Status)
	}
	if secret2 := string(credentials(t, gc.Name).Data[builds.KeyWebhookSecret]); secret2 != secret {
		t.Error("the webhook secret changed on a later reconcile")
	}

	// An App on a repository: its webhook appears, once.
	gitApp(t, "gitconn-a", "api", fake.URL+"/acme/api.git", gc.Name)
	gitApp(t, "gitconn-a", "api-ssh", "ssh://git@"+strings.TrimPrefix(fake.URL, "https://")+"/acme/api.git", gc.Name) // same repository
	gitApp(t, "gitconn-a", "elsewhere", "https://example.com/acme/api", gc.Name)                                      // not this host
	gitReconcile(t, r, gc.Name)
	gitReconcile(t, r, gc.Name)
	hooks := fake.Hooks()
	if len(hooks) != 1 || hooks[0].Repo != "acme/api" || hooks[0].Secret != secret || hooks[0].URL != getConnection(t, gc.Name).Status.WebhookURL {
		t.Fatalf("hooks = %+v", hooks)
	}
	got = getConnection(t, gc.Name)
	if len(got.Status.Webhooks) != 1 || !got.Status.Webhooks[0].Automatic || !got.Status.WebhookAutomatic || got.Status.WebhookFingerprint == "" {
		t.Errorf("webhooks = %+v", got.Status)
	}
	if strings.Contains(fmt.Sprint(got.Status), secret) {
		t.Error("the status carries the webhook secret")
	}

	// A rotated secret reaches the hook.
	setCredential(t, gc.Name, builds.KeyWebhookSecret, "rotated-secret")
	gitReconcile(t, r, gc.Name)
	if hooks := fake.Hooks(); len(hooks) != 1 || hooks[0].Secret != "rotated-secret" {
		t.Errorf("after rotation: %+v", hooks)
	}

	// A token without the webhook permission: add it by hand.
	fake.DenyHooks(true)
	gitApp(t, "gitconn-a", "web", fake.URL+"/acme/web", gc.Name)
	gitReconcile(t, r, gc.Name)
	got = getConnection(t, gc.Name)
	if got.Status.WebhookAutomatic || len(got.Status.Webhooks) != 2 || got.Status.Webhooks[1].Automatic || got.Status.Webhooks[1].Message == "" {
		t.Errorf("denied hooks: %+v", got.Status)
	}
	if c := ready(t, got); c.Status != metav1.ConditionTrue || !strings.Contains(c.Message, "by hand") {
		t.Errorf("ready with manual hooks: %+v", c)
	}
	fake.DenyHooks(false)

	// Projects: apps elsewhere are not this connection's business.
	got.Spec.Projects = []string{"someone-else"}
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	gitReconcile(t, r, gc.Name)
	if got := getConnection(t, gc.Name); len(got.Status.Webhooks) != 0 {
		t.Errorf("webhooks outside the connection's projects: %+v", got.Status.Webhooks)
	}

	// A revoked token.
	setCredential(t, gc.Name, builds.KeyToken, "revoked")
	gitReconcile(t, r, gc.Name)
	got = getConnection(t, gc.Name)
	if c := ready(t, got); c.Status != metav1.ConditionFalse || c.Reason != "CredentialsRejected" || got.Status.Account != "" || strings.Contains(c.Message, "revoked") {
		t.Errorf("revoked: %+v %q", c, got.Status.Account)
	}

	// Deleting the connection removes its Secret and its line in the Role.
	if err := k8s.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		err := k8s.Get(ctx, client.ObjectKey{Name: gc.Name}, &kwerftv1.GitConnection{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("still there: %v", err)
	})
	gitReconcile(t, r, gc.Name)
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: "git-gh-token"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("secret after delete: %v", err)
	}
	for _, rule := range credentialsRole(t).Rules {
		if slices.Contains(rule.ResourceNames, "git-gh-token") || len(rule.ResourceNames) == 0 {
			t.Errorf("role after delete: %+v", rule)
		}
	}
}

func TestGitConnectionGitHubAppSetsItsWebhook(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ensureNamespace(t, builds.Namespace)
	fake := newGitFake(t)
	r := &GitConnectionReconciler{Client: k8s, Git: fake.Factory(), ConsoleDomain: testConsoleDomain}
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "gh-app"},
		Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitHub, URL: fake.URL, Auth: kwerftv1.GitAuthGitHubApp,
			GitHubApp: &kwerftv1.GitHubAppSettings{AppID: fake.AppID, InstallationID: fake.InstallationID}}}
	if err := k8s.Create(ctx, gc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc) })
	gitReconcile(t, r, gc.Name)
	setCredential(t, gc.Name, builds.KeyGitHubAppKey, string(fake.AppKeyPEM))
	gitReconcile(t, r, gc.Name)
	got := getConnection(t, gc.Name)
	if got.Status.Account != "kwerft-test[bot] on acme" || !got.Status.WebhookAutomatic || ready(t, got).Status != metav1.ConditionTrue {
		t.Errorf("status = %+v", got.Status)
	}
	hooks := fake.Hooks()
	if len(hooks) != 1 || hooks[0].Provider != "app" || hooks[0].URL != got.Status.WebhookURL ||
		hooks[0].Secret != string(credentials(t, gc.Name).Data[builds.KeyWebhookSecret]) {
		t.Errorf("App hook = %+v", hooks)
	}
	// Unchanged: the App's hook is not rewritten on every check.
	n := len(fake.Requests())
	gitReconcile(t, r, gc.Name)
	for _, req := range fake.Requests()[n:] {
		if strings.HasPrefix(req, "PATCH") {
			t.Errorf("rewrote the hook: %s", req)
		}
	}
}

func TestGitConnectionDeployKeyRecordsHostKey(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ensureNamespace(t, builds.Namespace)
	scanned := ""
	r := &GitConnectionReconciler{Client: k8s, ConsoleDomain: testConsoleDomain,
		ScanHostKey: func(_ context.Context, addr string) (string, string, error) {
			scanned = addr
			return "git.example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl", "SHA256:test", nil
		}}
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "deploy-key"},
		Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.Generic, URL: "https://git.example.com", Auth: kwerftv1.GitAuthSSHKey}}
	if err := k8s.Create(ctx, gc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc) })
	gitReconcile(t, r, gc.Name)
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(key, "")
	setCredential(t, gc.Name, builds.KeySSHPrivateKey, string(pem.EncodeToMemory(block)))
	gitApp(t, builds.Namespace, "dk-app", "git@git.example.com:team/app.git", gc.Name)
	gitReconcile(t, r, gc.Name)
	got := getConnection(t, gc.Name)
	if scanned != "git.example.com:22" || !strings.HasPrefix(string(credentials(t, gc.Name).Data[builds.KeyKnownHosts]), "git.example.com ssh-ed25519") {
		t.Errorf("host key not recorded: %q", scanned)
	}
	if !strings.HasPrefix(got.Status.Account, "deploy key SHA256:") || got.Status.WebhookAutomatic || len(got.Status.Webhooks) != 1 ||
		got.Status.Webhooks[0].Automatic || !strings.Contains(got.Status.Webhooks[0].Message, "by hand") {
		t.Errorf("status = %+v", got.Status)
	}
}

func TestGitConnectionNeverAdoptsOldCredentials(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	ensureNamespace(t, builds.Namespace)
	r := &GitConnectionReconciler{Client: k8s, ConsoleDomain: testConsoleDomain}
	// A Secret left behind by an earlier connection "reused" (another UID),
	// with that connection's token.
	old := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: "git-reused",
		Labels: map[string]string{LabelGitConnection: "reused"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: kwerftv1.GroupVersion.String(), Kind: "GitConnection", Name: "reused",
			UID: "00000000-0000-0000-0000-000000000001", Controller: ptrTo(true)}}},
		Data: map[string][]byte{builds.KeyToken: []byte("old-token"), builds.KeyWebhookSecret: []byte("old")}}
	if err := k8s.Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "reused"},
		Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitLab, URL: "https://gitlab.example.com", Auth: kwerftv1.GitAuthToken}}
	if err := k8s.Create(ctx, gc); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc) })
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: gc.Name}}); err == nil {
		t.Error("the old Secret was used")
	}
	gitReconcile(t, r, gc.Name)
	sec := credentials(t, gc.Name)
	if len(sec.Data[builds.KeyToken]) != 0 || string(sec.Data[builds.KeyWebhookSecret]) == "old" || !metav1.IsControlledBy(sec, getConnection(t, gc.Name)) {
		t.Errorf("secret = %v %+v", sec.Data, sec.OwnerReferences)
	}

	// A Secret of that name that is not a connection's: left alone, reported.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: builds.Namespace, Name: "git-foreign"}}
	if err := k8s.Create(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	gc2 := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: "foreign"},
		Spec: kwerftv1.GitConnectionSpec{Provider: kwerftv1.GitLab, URL: "https://gitlab.example.com", Auth: kwerftv1.GitAuthNone}}
	if err := k8s.Create(ctx, gc2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), gc2) })
	gitReconcile(t, r, gc2.Name)
	if c := ready(t, getConnection(t, gc2.Name)); c.Reason != "SecretConflict" {
		t.Errorf("foreign secret: %+v", c)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(foreign), &corev1.Secret{}); err != nil {
		t.Errorf("the foreign Secret was touched: %v", err)
	}
}
