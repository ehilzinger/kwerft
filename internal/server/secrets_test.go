package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// text sends a request and returns the status and the body as text, for
// checks that a value appears nowhere in an answer.
func (s *session) text(t *testing.T, method, path string, body any, header ...string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.url+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	c := s.client
	if req.Header.Get("Authorization") != "" {
		c = http.DefaultClient // the token alone, no cookies
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(out)
}

func setsPath(project string, rest ...string) string {
	return "/api/v1/projects/" + project + "/secret-sets" + strings.Join(append([]string{""}, rest...), "/")
}

// setAt polls a set through the API until check passes.
func setAt(t *testing.T, s *session, project, name string, check func(secretSetJSON) error) secretSetJSON {
	t.Helper()
	var set secretSetJSON
	eventually(t, func() error {
		set = secretSetJSON{}
		if code := s.do(t, "GET", setsPath(project, name), nil, &set); code != http.StatusOK {
			return fmt.Errorf("get set: %d", code)
		}
		return check(set)
	})
	return set
}

func hasKey(name, by, source string) func(secretSetJSON) error {
	return func(s secretSetJSON) error {
		for _, k := range s.Keys {
			if k.Name == name {
				if k.UpdatedBy != by || k.Source != source || k.UpdatedAt == nil {
					return fmt.Errorf("key %s = %+v, want by %q source %s", name, k, by, source)
				}
				return nil
			}
		}
		return fmt.Errorf("no key %s in %+v", name, s.Keys)
	}
}

func secretData(t *testing.T, project, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: project, Name: name}, &sec); err != nil {
		t.Fatal(err)
	}
	return sec.Data
}

func TestSecretValuesAreWriteOnly(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-a")
	const value = "sk_live_write_only_4242"

	var created secretSetJSON
	if code := c.dev.do(t, "POST", setsPath("vault-a"), map[string]any{"name": "payments", "description": "Stripe live account"}, &created); code != http.StatusCreated {
		t.Fatalf("developer creates a set: %d", code)
	}
	// Right after creating it: the API waits for the Secret and the Role.
	code, body := c.dev.text(t, "PUT", setsPath("vault-a", "payments", "keys", "STRIPE_KEY"), map[string]any{"value": value})
	if code != http.StatusOK || strings.Contains(body, value) {
		t.Fatalf("developer sets a value: %d %s", code, body)
	}
	if got := secretData(t, "vault-a", "payments")["STRIPE_KEY"]; string(got) != value {
		t.Fatalf("stored %q", got)
	}
	setAt(t, c.viewer, "vault-a", "payments", hasKey("STRIPE_KEY", "developer@example.com", controllers.SourceSet))

	// Nothing that lists or reads sets carries the value, for any role.
	for _, s := range []*session{c.owner, c.dev, c.viewer} {
		for _, path := range []string{setsPath("vault-a"), setsPath("vault-a", "payments")} {
			code, body := s.text(t, "GET", path, nil)
			if code != http.StatusOK || strings.Contains(body, value) || !strings.Contains(body, "STRIPE_KEY") {
				t.Errorf("GET %s: %d %s", path, code, body)
			}
		}
	}
	// Nor does the audit log, which names the set and key.
	entries, err := c.store.RecentAudit(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Detail+e.Target, value) {
			t.Errorf("audit entry %+v carries the value", e)
		}
		found = found || e.Action == "secret.set" && e.Target == "vault-a/payments/STRIPE_KEY" && e.Actor == "developer@example.com"
	}
	if !found {
		t.Error("no secret.set audit entry")
	}

	// Kubernetes itself: the developer patches, never reads.
	dev, err := cluster.imp.For("developer@example.com", "developer")
	if err != nil {
		t.Fatal(err)
	}
	if err := dev.Get(context.Background(), client.ObjectKey{Namespace: "vault-a", Name: "payments"}, &corev1.Secret{}); !apierrors.IsForbidden(err) {
		t.Errorf("developer reads the Secret: %v, want Forbidden", err)
	}
	if err := dev.List(context.Background(), &corev1.SecretList{}, client.InNamespace("vault-a")); !apierrors.IsForbidden(err) {
		t.Errorf("developer lists Secrets: %v, want Forbidden", err)
	}

	// Viewers see key names, set nothing.
	if code := c.viewer.do(t, "PUT", setsPath("vault-a", "payments", "keys", "STRIPE_KEY"), map[string]any{"value": "x"}, nil); code != http.StatusForbidden {
		t.Errorf("viewer sets a value: %d, want 403", code)
	}
	if code := c.viewer.do(t, "POST", setsPath("vault-a"), map[string]any{"name": "mine"}, nil); code != http.StatusForbidden {
		t.Errorf("viewer creates a set: %d, want 403", code)
	}

	// Generate, remove.
	var rec secretKeyJSON
	if code := c.dev.do(t, "PUT", setsPath("vault-a", "payments", "keys", "WEBHOOK_SECRET"), map[string]any{"generate": true}, &rec); code != http.StatusOK || rec.Source != controllers.SourceGenerated {
		t.Fatalf("generate: %d %+v", code, rec)
	}
	if got := secretData(t, "vault-a", "payments")["WEBHOOK_SECRET"]; len(got) != 43 {
		t.Errorf("generated %d characters, want 43", len(got))
	}
	if code := c.dev.do(t, "DELETE", setsPath("vault-a", "payments", "keys", "WEBHOOK_SECRET"), nil, nil); code != http.StatusNoContent {
		t.Errorf("remove a key: %d", code)
	}
	if _, ok := secretData(t, "vault-a", "payments")["WEBHOOK_SECRET"]; ok {
		t.Error("the key is still there")
	}
	setAt(t, c.dev, "vault-a", "payments", func(s secretSetJSON) error {
		if len(s.Keys) != 1 {
			return fmt.Errorf("keys %+v", s.Keys)
		}
		return nil
	})
	for _, bad := range []map[string]any{{}, {"value": ""}, {"value": "x", "generate": true}} {
		if code := c.dev.do(t, "PUT", setsPath("vault-a", "payments", "keys", "X"), bad, nil); code != http.StatusUnprocessableEntity {
			t.Errorf("PUT %v: %d, want 422", bad, code)
		}
	}
	if code := c.dev.do(t, "PUT", setsPath("vault-a", "payments", "keys", "a%2Fb"), map[string]any{"value": "x"}, nil); code != http.StatusUnprocessableEntity {
		t.Errorf("a key with a slash: %d, want 422", code)
	}
}

func TestSecretRevealIsForOwnersAndAdminsAfterTheirPassword(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-r")
	const value = "reveal-me-9000"
	if code := c.dev.do(t, "POST", setsPath("vault-r"), map[string]any{"name": "db"}, nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	if code := c.dev.do(t, "PUT", setsPath("vault-r", "db", "keys", "PASSWORD"), map[string]any{"value": value}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	reveal := setsPath("vault-r", "db", "keys", "PASSWORD", "reveal")
	for _, s := range []*session{c.dev, c.viewer} {
		if code, body := s.text(t, "POST", reveal, map[string]string{"password": "a long test password"}); code != http.StatusForbidden || strings.Contains(body, value) {
			t.Errorf("reveal as a member: %d %s", code, body)
		}
	}
	if code, body := c.owner.text(t, "POST", reveal, map[string]string{"password": "wrong"}); code != http.StatusBadRequest || strings.Contains(body, value) {
		t.Errorf("reveal with a wrong password: %d %s", code, body)
	}
	if code, body := c.owner.text(t, "POST", reveal, map[string]string{}); code != http.StatusBadRequest || strings.Contains(body, value) {
		t.Errorf("reveal without a password: %d %s", code, body)
	}
	var out map[string]string
	if code := c.owner.do(t, "POST", reveal, map[string]string{"password": "a long test password"}, &out); code != http.StatusOK || out["value"] != value {
		t.Fatalf("owner reveals: %d %v", code, out)
	}
	if a := lastAudit(t, c.store, "secret.reveal"); a == nil || a.Target != "vault-r/db/PASSWORD" || a.Actor != "owner@example.com" || strings.Contains(a.Detail, value) {
		t.Errorf("audit %+v", a)
	}
	if code := c.owner.do(t, "POST", setsPath("vault-r", "db", "keys", "NOPE", "reveal"), map[string]string{"password": "a long test password"}, nil); code != http.StatusNotFound {
		t.Errorf("reveal a missing key: %d, want 404", code)
	}

	// An owner's API token never reveals.
	var tok struct {
		Token string `json:"token"`
	}
	if code := c.owner.do(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "ci", "role": "owner"}, &tok); code != http.StatusCreated {
		t.Fatalf("token: %d", code)
	}
	code, body := c.owner.text(t, "POST", reveal, map[string]string{"password": "a long test password"}, "Authorization", "Bearer "+tok.Token)
	if code != http.StatusForbidden || strings.Contains(body, value) {
		t.Errorf("reveal with a token: %d %s", code, body)
	}
	// The token sets values all the same.
	if code, _ := c.owner.text(t, "PUT", setsPath("vault-r", "db", "keys", "OTHER"), map[string]string{"value": "v"}, "Authorization", "Bearer "+tok.Token); code != http.StatusOK {
		t.Errorf("set with a token: %d", code)
	}
}

func TestSecretSetGeneratedAndDerivedKeys(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-g")
	body := map[string]any{"name": "postgres-main", "generate": []string{"PASSWORD"},
		"derived": []map[string]string{{"key": "DATABASE_URL", "template": "postgres://app:${PASSWORD}@postgres-main:5432/app"}}}
	if code := c.dev.do(t, "POST", setsPath("vault-g"), body, nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	setAt(t, c.dev, "vault-g", "postgres-main", func(s secretSetJSON) error {
		if s.Phase != "ready" || len(s.Keys) != 2 {
			return fmt.Errorf("phase %s keys %+v", s.Phase, s.Keys)
		}
		return nil
	})
	data := secretData(t, "vault-g", "postgres-main")
	if string(data["DATABASE_URL"]) != "postgres://app:"+string(data["PASSWORD"])+"@postgres-main:5432/app" {
		t.Errorf("derived %q", data["DATABASE_URL"])
	}
	for _, req := range []struct{ method, key string }{{"PUT", "DATABASE_URL"}, {"DELETE", "DATABASE_URL"}, {"DELETE", "PASSWORD"}} {
		var b any
		if req.method == "PUT" {
			b = map[string]string{"value": "x"}
		}
		if code := c.dev.do(t, req.method, setsPath("vault-g", "postgres-main", "keys", req.key), b, nil); code != http.StatusConflict {
			t.Errorf("%s %s: %d, want 409", req.method, req.key, code)
		}
	}
	// A new password: the URL follows.
	if code := c.dev.do(t, "PUT", setsPath("vault-g", "postgres-main", "keys", "PASSWORD"), map[string]any{"generate": true}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	eventually(t, func() error {
		d := secretData(t, "vault-g", "postgres-main")
		if string(d["PASSWORD"]) == string(data["PASSWORD"]) || !strings.Contains(string(d["DATABASE_URL"]), string(d["PASSWORD"])) {
			return fmt.Errorf("URL does not follow the new password yet")
		}
		return nil
	})
	for _, bad := range []map[string]any{
		{"name": "x1", "generate": []string{"A", "A"}},
		{"name": "x2", "derived": []map[string]string{{"key": "U", "template": "no refs"}}},
		{"name": "x3", "derived": []map[string]string{{"key": "U", "template": "${U}"}}},
		{"name": "x4", "generate": []string{"U"}, "derived": []map[string]string{{"key": "U", "template": "${A}"}}},
		{"name": "Bad_Name"},
	} {
		if code := c.dev.do(t, "POST", setsPath("vault-g"), bad, nil); code != http.StatusUnprocessableEntity {
			t.Errorf("create %v: %d, want 422", bad, code)
		}
	}
	// Change the lists.
	var updated secretSetJSON
	if code := c.dev.do(t, "PATCH", setsPath("vault-g", "postgres-main"), map[string]any{"generate": []string{"PASSWORD", "ADMIN_PASSWORD"}}, &updated); code != http.StatusOK ||
		!slices.Equal(updated.Generate, []string{"PASSWORD", "ADMIN_PASSWORD"}) || len(updated.Derived) != 1 {
		t.Fatalf("patch: %d %+v", code, updated)
	}
	setAt(t, c.dev, "vault-g", "postgres-main", hasKey("ADMIN_PASSWORD", "", controllers.SourceGenerated))
}

func TestSecretSetNeverTakesOverASecret(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-c")
	if err := cluster.admin.Create(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "vault-c", Name: "legacy"},
		StringData: map[string]string{"token": "keep"}}); err != nil {
		t.Fatal(err)
	}
	if code := c.dev.do(t, "POST", setsPath("vault-c"), map[string]any{"name": "legacy"}, nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	setAt(t, c.dev, "vault-c", "legacy", func(s secretSetJSON) error {
		if s.Phase != "failed" || s.Reason != "Conflict" {
			return fmt.Errorf("phase %s reason %s", s.Phase, s.Reason)
		}
		return nil
	})
	for _, s := range []*session{c.dev, c.owner} {
		if code := s.do(t, "PUT", setsPath("vault-c", "legacy", "keys", "token"), map[string]any{"value": "overwrite"}, nil); code != http.StatusConflict {
			t.Errorf("write into a foreign Secret: %d, want 409", code)
		}
	}
	if got := secretData(t, "vault-c", "legacy")["token"]; string(got) != "keep" {
		t.Errorf("the foreign Secret changed: %q", got)
	}
	// Kubernetes agrees: not in the Role, so not even a direct patch.
	dev, _ := cluster.imp.For("developer@example.com", "developer")
	err := dev.Patch(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "vault-c", Name: "legacy"}},
		client.RawPatch("application/merge-patch+json", []byte(`{"data":{"token":"eA=="}}`)))
	if !apierrors.IsForbidden(err) {
		t.Errorf("developer patches the foreign Secret: %v, want Forbidden", err)
	}
}

// TestAppEnvSecretToggle: the env editor's "secret" toggle writes into the
// App's own set <app>-env, created on the first key and owned by the App;
// the App waits for the value (SecretMissing), then rolls when it changes.
func TestAppEnvSecretToggle(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-e")
	app := imageApp("api", "nginx:1.27")
	app["spec"].(map[string]any)["env"] = []map[string]any{
		{"name": "SESSION_SECRET", "valueFrom": map[string]any{"secretKeyRef": map[string]string{"name": "api-env", "key": "SESSION_SECRET"}}},
	}
	if code := c.dev.do(t, "POST", "/api/v1/projects/vault-e/apps", app, nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	appAt(t, c.dev, "vault-e", "api", func(a *kwerftv1.App) error {
		if r := appReadyReason(a); r != "SecretMissing" {
			return fmt.Errorf("reason %q", r)
		}
		return nil
	})
	if code := c.dev.do(t, "PUT", setsPath("vault-e", "api-env", "keys", "SESSION_SECRET"), map[string]any{"generate": true}, nil); code != http.StatusOK {
		t.Fatalf("first key of the app's own set: %d", code)
	}
	set := setAt(t, c.dev, "vault-e", "api-env", func(s secretSetJSON) error {
		if !slices.Equal(s.UsedBy, []string{"App/api"}) || len(s.Keys) != 1 {
			return fmt.Errorf("usedBy %v keys %+v", s.UsedBy, s.Keys)
		}
		return nil
	})
	if set.App != "api" {
		t.Errorf("set owner %q, want the App", set.App)
	}
	var hash string
	deployment(t, "vault-e", "api", func(d *appsv1.Deployment) error {
		if hash = d.Spec.Template.Annotations[controllers.AnnotationSecretsHash]; hash == "" {
			return fmt.Errorf("no secrets hash")
		}
		return nil
	})
	if code := c.dev.do(t, "PUT", setsPath("vault-e", "api-env", "keys", "SESSION_SECRET"), map[string]any{"value": "rotated"}, nil); code != http.StatusOK {
		t.Fatal(code)
	}
	deployment(t, "vault-e", "api", func(d *appsv1.Deployment) error {
		if d.Spec.Template.Annotations[controllers.AnnotationSecretsHash] == hash {
			return fmt.Errorf("not rolled yet")
		}
		return nil
	})
	// Only <app>-env of an existing App is created on the fly.
	if code := c.dev.do(t, "PUT", setsPath("vault-e", "nope-env", "keys", "K"), map[string]any{"value": "v"}, nil); code != http.StatusNotFound {
		t.Errorf("a set of no App: %d, want 404", code)
	}
}

func appReadyReason(a *kwerftv1.App) string {
	for _, c := range a.Status.Conditions {
		if c.Type == controllers.ConditionReady && c.ObservedGeneration == a.Generation {
			return c.Reason
		}
	}
	return ""
}

func TestSecretSetCopyToAnotherProject(t *testing.T) {
	c := newConsole(t)
	c.project(t, "vault-from")
	c.project(t, "vault-to")
	if code := c.dev.do(t, "POST", setsPath("vault-from"), map[string]any{"name": "s3", "description": "Object Storage"}, nil); code != http.StatusCreated {
		t.Fatal(code)
	}
	for k, v := range map[string]string{"ACCESS_KEY": "AK", "SECRET_KEY": "SK"} {
		if code := c.dev.do(t, "PUT", setsPath("vault-from", "s3", "keys", k), map[string]any{"value": v}, nil); code != http.StatusOK {
			t.Fatal(code)
		}
	}
	if code := c.dev.do(t, "POST", setsPath("vault-from", "s3", "copy"), map[string]any{"project": "vault-to"}, nil); code != http.StatusForbidden {
		t.Errorf("developer copies: %d, want 403", code)
	}
	code, body := c.owner.text(t, "POST", setsPath("vault-from", "s3", "copy"), map[string]any{"project": "vault-to"})
	if code != http.StatusCreated || strings.Contains(body, `"SK"`) || strings.Contains(body, `"AK"`) {
		t.Fatalf("owner copies: %d %s", code, body)
	}
	got := secretData(t, "vault-to", "s3")
	if string(got["ACCESS_KEY"]) != "AK" || string(got["SECRET_KEY"]) != "SK" {
		t.Errorf("copied values %v", got)
	}
	setAt(t, c.dev, "vault-to", "s3", hasKey("SECRET_KEY", "owner@example.com", controllers.SourceSet))
	if a := lastAudit(t, c.store, "secret_set.copy"); a == nil || a.Target != "vault-from/s3" || !strings.Contains(a.Detail, "vault-to/s3") {
		t.Errorf("audit %+v", a)
	}
	if code := c.owner.do(t, "POST", setsPath("vault-from", "s3", "copy"), map[string]any{"project": "vault-to"}, nil); code != http.StatusConflict {
		t.Errorf("copy onto an existing set: %d, want 409", code)
	}
}
