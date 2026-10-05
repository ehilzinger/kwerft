package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func createSecretSet(t *testing.T, ns, name string, spec kwerftv1.SecretSetSpec) *kwerftv1.SecretSet {
	t.Helper()
	s := &kwerftv1.SecretSet{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: spec}
	if err := k8s.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	return s
}

// waitForSet waits until the set's Ready condition is for its generation
// and check passes.
func waitForSet(t *testing.T, s *kwerftv1.SecretSet, check func(*kwerftv1.SecretSet) error) *kwerftv1.SecretSet {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(s), s); err != nil {
			return err
		}
		if _, err := readyReason(s.Status.Conditions, s.Generation); err != nil {
			return err
		}
		return check(s)
	})
	return s
}

func reasonIs(want string) func(*kwerftv1.SecretSet) error {
	return func(s *kwerftv1.SecretSet) error {
		if r, _ := readyReason(s.Status.Conditions, s.Generation); r != want {
			return fmt.Errorf("reason %q, want %q (%+v)", r, want, s.Status.Conditions)
		}
		return nil
	}
}

// setValue writes a key as the console would: value and record together.
func setValue(t *testing.T, ns, secret, key, value, by string) {
	t.Helper()
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"annotations": map[string]string{KeyAnnotation(key): KeyRecord{At: metav1.Now().Time, By: by, Source: SourceSet}.Encode()}},
		"data":     map[string][]byte{key: []byte(value)},
	})
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: secret}}
	if err := k8s.Patch(context.Background(), sec, client.RawPatch(types.MergePatchType, patch)); err != nil {
		t.Fatal(err)
	}
}

func TestSecretSetOwnsItsSecretAndFillsKeys(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "vault")
	set := createSecretSet(t, "vault", "postgres-main", kwerftv1.SecretSetSpec{
		Generate: []string{"PASSWORD"},
		Derived: []kwerftv1.DerivedSecretKey{
			{Key: "DATABASE_URL", Template: "postgres://app:${PASSWORD}@postgres-main:5432/app"},
			{Key: "ADMIN_URL", Template: "https://admin:${ADMIN_PASSWORD}@example.com"},
		},
	})
	set = waitForSet(t, set, reasonIs("DerivedPending"))
	if msg := set.Status.Conditions[0].Message; !strings.Contains(msg, "ADMIN_URL waits for ADMIN_PASSWORD") {
		t.Errorf("message %q does not say what ADMIN_URL waits for", msg)
	}

	var sec corev1.Secret
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vault", Name: "postgres-main"}, &sec); err != nil {
		t.Fatal(err)
	}
	if !metav1.IsControlledBy(&sec, set) || sec.Labels[LabelSecretSet] != "postgres-main" {
		t.Errorf("secret not owned and labelled: %+v %+v", sec.OwnerReferences, sec.Labels)
	}
	pw := string(sec.Data["PASSWORD"])
	if len(pw) != 43 || strings.ContainsAny(pw, "+/=") {
		t.Errorf("generated password %q is not 32 bytes of base64url", pw)
	}
	if got := string(sec.Data["DATABASE_URL"]); got != "postgres://app:"+pw+"@postgres-main:5432/app" {
		t.Errorf("DATABASE_URL = %q", got)
	}
	if _, ok := sec.Data["ADMIN_URL"]; ok {
		t.Error("ADMIN_URL written before its input exists")
	}
	keys := map[string]kwerftv1.SecretKeyStatus{}
	for _, k := range set.Status.Keys {
		keys[k.Name] = k
	}
	if keys["PASSWORD"].Source != SourceGenerated || keys["DATABASE_URL"].Source != SourceDerived || keys["PASSWORD"].UpdatedAt == nil || len(keys) != 2 {
		t.Errorf("status keys = %+v", set.Status.Keys)
	}
	raw, _ := json.Marshal(set.Status)
	if strings.Contains(string(raw), pw) {
		t.Error("the status carries a value")
	}

	// A new input: derived keys follow; the user is recorded for theirs.
	setValue(t, "vault", "postgres-main", "PASSWORD", "hunter2", "dana@example.com")
	setValue(t, "vault", "postgres-main", "ADMIN_PASSWORD", "s3cret", "dana@example.com")
	waitForSet(t, set, func(s *kwerftv1.SecretSet) error {
		if r, _ := readyReason(s.Status.Conditions, s.Generation); r != "Ready" {
			return fmt.Errorf("reason %q", r)
		}
		if len(s.Status.Keys) != 4 {
			return fmt.Errorf("keys %+v", s.Status.Keys)
		}
		return nil
	})
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&sec), &sec); err != nil {
		t.Fatal(err)
	}
	if got := string(sec.Data["DATABASE_URL"]); got != "postgres://app:hunter2@postgres-main:5432/app" {
		t.Errorf("DATABASE_URL did not follow PASSWORD: %q", got)
	}
	if got := string(sec.Data["ADMIN_URL"]); got != "https://admin:s3cret@example.com" {
		t.Errorf("ADMIN_URL = %q", got)
	}
	for _, k := range set.Status.Keys {
		if k.Name == "PASSWORD" && (k.UpdatedBy != "dana@example.com" || k.Source != SourceSet) {
			t.Errorf("PASSWORD record = %+v", k)
		}
	}

	// A generated key is only filled while missing.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(&sec), &sec); err != nil || string(sec.Data["PASSWORD"]) != "hunter2" {
		t.Errorf("PASSWORD regenerated: %q (err=%v)", sec.Data["PASSWORD"], err)
	}

	// Roles: patch and get on exactly this Secret.
	var role rbacv1.Role
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vault", Name: SecretSetsRole}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"postgres-main"}) || !slices.Equal(role.Rules[0].Verbs, []string{"patch"}) ||
			!slices.Equal(role.Rules[0].Resources, []string{"secrets"}) {
			return fmt.Errorf("role rules %+v", role.Rules)
		}
		return nil
	})
	var read rbacv1.Role
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vault", Name: SecretSetsReadRole}, &read); err != nil {
		t.Fatal(err)
	}
	if len(read.Rules) != 1 || !slices.Equal(read.Rules[0].Verbs, []string{"get"}) || !slices.Equal(read.Rules[0].ResourceNames, []string{"postgres-main"}) {
		t.Errorf("read role rules %+v", read.Rules)
	}
	subjects := func(name string) []string {
		var rb rbacv1.RoleBinding
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vault", Name: name}, &rb); err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, s := range rb.Subjects {
			out = append(out, s.Kind+":"+s.Name)
		}
		return out
	}
	if got := subjects(SecretSetsRole); !slices.Equal(got, []string{"Group:kwerft:role:owner", "Group:kwerft:role:admin", "Group:kwerft:role:developer"}) {
		t.Errorf("writers = %v", got)
	}
	if got := subjects(SecretSetsReadRole); !slices.Equal(got, []string{"Group:kwerft:role:owner", "Group:kwerft:role:admin"}) {
		t.Errorf("readers = %v", got)
	}

	// Deleting the set takes it out of the Roles (envtest has no garbage
	// collector, so the Secret itself stays here).
	if err := k8s.Delete(ctx, set); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "vault", Name: SecretSetsRole}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 0 {
			return fmt.Errorf("role still names %+v", role.Rules)
		}
		return nil
	})
}

func TestSecretSetNeverAdoptsASecret(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "adopt")
	if err := k8s.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "adopt", Name: "legacy"}, StringData: map[string]string{"token": "keep"}}); err != nil {
		t.Fatal(err)
	}
	other := createSecretSet(t, "adopt", "other", kwerftv1.SecretSetSpec{Generate: []string{"X"}})
	set := createSecretSet(t, "adopt", "legacy", kwerftv1.SecretSetSpec{Generate: []string{"PASSWORD"}})
	set = waitForSet(t, set, reasonIs("Conflict"))
	if len(set.Status.Keys) != 0 {
		t.Errorf("a foreign Secret's keys are listed: %+v", set.Status.Keys)
	}
	var sec corev1.Secret
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "adopt", Name: "legacy"}, &sec); err != nil {
		t.Fatal(err)
	}
	if len(sec.Data) != 1 || len(sec.OwnerReferences) != 0 || sec.Labels[LabelSecretSet] != "" {
		t.Errorf("the foreign Secret was changed: %+v", sec)
	}
	waitForSet(t, other, reasonIs("Ready"))
	var role rbacv1.Role
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "adopt", Name: SecretSetsRole}, &role); err != nil {
			return err
		}
		if len(role.Rules) != 1 || !slices.Equal(role.Rules[0].ResourceNames, []string{"other"}) {
			return fmt.Errorf("role rules %+v, want only other", role.Rules)
		}
		return nil
	})
}

func TestSecretSetMembersProjectBindsItsDevelopers(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createProject(t, "closed", kwerftv1.ProjectSpec{Access: kwerftv1.ProjectAccessMembers, Members: []kwerftv1.ProjectMember{
		{User: "dev@example.com", Role: "developer"}, {User: "view@example.com", Role: "viewer"},
	}})
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: "closed"}, &corev1.Namespace{}) })
	waitForSet(t, createSecretSet(t, "closed", "payments", kwerftv1.SecretSetSpec{}), reasonIs("Ready"))
	eventually(t, func() error {
		var rb rbacv1.RoleBinding
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "closed", Name: SecretSetsRole}, &rb); err != nil {
			return err
		}
		var got []string
		for _, s := range rb.Subjects {
			got = append(got, s.Kind+":"+s.Name)
		}
		if !slices.Equal(got, []string{"Group:kwerft:role:owner", "Group:kwerft:role:admin", "User:kwerft:dev@example.com"}) {
			return fmt.Errorf("writers %v", got)
		}
		return nil
	})
}

func TestSecretSetReportsUsersAndMissingKeys(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "users")
	set := createSecretSet(t, "users", "mailer", kwerftv1.SecretSetSpec{})
	waitForSet(t, set, reasonIs("Ready"))
	setValue(t, "users", "mailer", "SMTP_PASSWORD", "pw", "dana@example.com")

	spec := imageApp("nginx:1.29")
	spec.Env = []corev1.EnvVar{
		{Name: "SMTP_PASSWORD", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "mailer"}, Key: "SMTP_PASSWORD"}}},
		{Name: "SMTP_USER", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "mailer"}, Key: "SMTP_USER"}}},
	}
	createApp(t, "users", "notifier", spec)
	files := imageApp("alpine:3.22")
	files.Volumes = []kwerftv1.AppVolume{{Path: "/etc/mail", Secret: "mailer"}}
	createApp(t, "users", "reader", files)

	waitForSet(t, set, func(s *kwerftv1.SecretSet) error {
		if !slices.Equal(s.Status.UsedBy, []string{"App/notifier", "App/reader"}) {
			return fmt.Errorf("usedBy %v", s.Status.UsedBy)
		}
		if !slices.Equal(s.Status.Missing, []string{"App/notifier: SMTP_USER"}) {
			return fmt.Errorf("missing %v", s.Status.Missing)
		}
		if r, _ := readyReason(s.Status.Conditions, s.Generation); r != "MissingKeys" {
			return fmt.Errorf("reason %q", r)
		}
		return nil
	})
}

// TestAppRollsWhenASecretValueChanges: an App waits (SecretMissing) for a
// key its env references instead of starting pods that cannot, and a new
// value rolls it through the pod template without a new revision.
func TestAppRollsWhenASecretValueChanges(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "rolling")
	set := createSecretSet(t, "rolling", "payments", kwerftv1.SecretSetSpec{})
	waitForSet(t, set, reasonIs("Ready"))

	spec := imageApp("nginx:1.29")
	spec.Env = []corev1.EnvVar{
		{Name: "PLAIN", Value: "1"},
		{Name: "STRIPE_KEY", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "payments"}, Key: "STRIPE_KEY"}}},
	}
	app := createApp(t, "rolling", "api", spec)
	app = waitForApp(t, app, "SecretMissing")
	if msg := app.Status.Conditions[0].Message; !strings.Contains(msg, "payments has no key STRIPE_KEY") {
		t.Errorf("message %q does not name the key", msg)
	}
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rolling", Name: "api"}, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("a Deployment exists before its secret value (err=%v)", err)
	}

	setValue(t, "rolling", "payments", "STRIPE_KEY", "sk_live_1", "dana@example.com")
	var first string
	eventually(t, func() error {
		var d appsv1.Deployment
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rolling", Name: "api"}, &d); err != nil {
			return err
		}
		first = d.Spec.Template.Annotations[AnnotationSecretsHash]
		if first == "" {
			return fmt.Errorf("no secrets hash on the pod template")
		}
		return nil
	})
	app = waitForApp(t, app, "Progressing")
	revision := app.Status.Revision

	setValue(t, "rolling", "payments", "STRIPE_KEY", "sk_live_2", "dana@example.com")
	eventually(t, func() error {
		var d appsv1.Deployment
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rolling", Name: "api"}, &d); err != nil {
			return err
		}
		if h := d.Spec.Template.Annotations[AnnotationSecretsHash]; h == first {
			return fmt.Errorf("hash unchanged")
		}
		return nil
	})
	// An unrelated key in the same Secret does not roll it.
	var d appsv1.Deployment
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rolling", Name: "api"}, &d); err != nil {
		t.Fatal(err)
	}
	second := d.Spec.Template.Annotations[AnnotationSecretsHash]
	setValue(t, "rolling", "payments", "UNUSED", "x", "dana@example.com")
	waitForSet(t, set, func(s *kwerftv1.SecretSet) error {
		if len(s.Status.Keys) != 2 {
			return fmt.Errorf("keys %+v", s.Status.Keys)
		}
		return nil
	})
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: "rolling", Name: "api"}, &d); err != nil || d.Spec.Template.Annotations[AnnotationSecretsHash] != second {
		t.Errorf("an unreferenced key changed the hash (err=%v)", err)
	}
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil || app.Status.Revision != revision {
		t.Errorf("revision %d, want %d: a value change is not a revision (err=%v)", app.Status.Revision, revision, err)
	}
	// The value itself is nowhere in the workload.
	raw, _ := json.Marshal(d.Spec.Template)
	if strings.Contains(string(raw), "sk_live") {
		t.Error("the pod template carries the value")
	}
}

func TestKeyAnnotation(t *testing.T) {
	for _, k := range []string{"PASSWORD", "tls.crt", "a", "_hidden", ".dot", strings.Repeat("K", 64), "x-"} {
		ann := KeyAnnotation(k)
		if !strings.HasPrefix(ann, KeyAnnotationPrefix) {
			t.Errorf("%s: %s", k, ann)
		}
		name := strings.TrimPrefix(ann, KeyAnnotationPrefix)
		if len(name) > 63 {
			t.Errorf("%s: annotation name %q too long", k, name)
		}
	}
	if KeyAnnotation("PASSWORD") != KeyAnnotationPrefix+"PASSWORD" {
		t.Error("a plain key is not used as is")
	}
	if KeyAnnotation("_a") == KeyAnnotation(".a") {
		t.Error("two keys share a record")
	}
}

func TestRenderTemplate(t *testing.T) {
	out, missing := renderTemplate("postgres://u:${PW}@h/${DB}?x=$NOT{}", map[string][]byte{"PW": []byte("p"), "DB": []byte("d")})
	if out != "postgres://u:p@h/d?x=$NOT{}" || len(missing) != 0 {
		t.Errorf("render = %q %v", out, missing)
	}
	if _, missing := renderTemplate("${A}${B}", map[string][]byte{"A": nil}); !slices.Equal(missing, []string{"B"}) {
		t.Errorf("missing = %v", missing)
	}
}
