package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/auth/oidctest"
	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Identity against the test cluster with the chart's RBAC: single sign-on
// settings written as the user, kubectl through the proxy with a downloaded
// kubeconfig, and data-key rotation writing its Secret.

func oidcSecretFixture(t *testing.T) {
	t.Helper()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: controllers.OIDCSecret}}
	if err := cluster.admin.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), sec) })
}

func TestSSOSettingsAreWrittenAsTheUser(t *testing.T) {
	requireCluster(t)
	iss := oidctest.New(t)
	settingsFixture(t)
	oidcSecretFixture(t)
	c := newConsole(t, func(cfg *Config) {
		cfg.SystemReader = cluster.admin // the console's own reads, as in production
		cfg.ssoHook = func(s *ssoAPI) { s.oidc.HTTP = iss.Client() }
	})
	body := map[string]any{"enabled": true, "provider": "keycloak", "issuer": iss.URL + "/", "clientId": iss.ClientID,
		"clientSecret": iss.ClientSecret, "allowedDomains": []string{"Example.com", "@example.com"}, "autoJoin": true, "defaultRole": "developer"}

	// Developers and viewers neither read nor write it.
	if code := c.dev.do(t, "PUT", "/api/v1/settings/sso", body, nil); code != http.StatusForbidden {
		t.Errorf("developer: %d", code)
	}
	if code := c.viewer.do(t, "GET", "/api/v1/settings/sso", nil, nil); code != http.StatusForbidden {
		t.Errorf("viewer reads: %d", code)
	}
	// Validation before anything is stored.
	var out map[string]any
	bad := map[string]any{"enabled": true, "provider": "keycloak", "issuer": iss.URL, "clientId": "x", "clientSecret": "y", "autoJoin": true}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/sso", bad, &out); code != http.StatusBadRequest || out["field"] != "allowedDomains" {
		t.Errorf("auto-join without domains: %d %v", code, out)
	}
	bad = map[string]any{"enabled": true, "provider": "oidc", "issuer": "https://127.0.0.1:1", "clientId": "x", "clientSecret": "y"}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/sso", bad, &out); code != http.StatusBadRequest || out["field"] != "issuer" {
		t.Errorf("unreachable issuer: %d %v", code, out)
	}
	bad = map[string]any{"enabled": true, "provider": "microsoft", "tenant": "common", "clientId": "x", "clientSecret": "y"}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/sso", bad, &out); code != http.StatusBadRequest || out["field"] != "tenant" {
		t.Errorf("multi-tenant Entra: %d %v", code, out)
	}

	out = nil
	if code := c.owner.do(t, "PUT", "/api/v1/settings/sso", body, &out); code != http.StatusOK {
		t.Fatalf("save: %d %v", code, out)
	}
	if out["secretSet"] != true || strings.Contains(stringify(out), iss.ClientSecret) {
		t.Errorf("response %v", out)
	}
	cs := settingsNow(t)
	if s := cs.Spec.SSO; s == nil || !s.Enabled || s.Issuer != iss.URL || s.ClientID != iss.ClientID ||
		strings.Join(s.AllowedDomains, ",") != "example.com" || !s.AutoJoin || s.DefaultRole != "developer" {
		t.Errorf("spec.sso %+v", cs.Spec.SSO)
	}
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.OIDCSecret}, &sec); err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[controllers.OIDCSecretKey]) != iss.ClientSecret {
		t.Errorf("secret %q", sec.Data[controllers.OIDCSecretKey])
	}
	// Write-only, also for owners: patch, never get.
	if canName(t, "owner", controllers.GatewayNamespace, "get", "", "secrets", "", controllers.OIDCSecret) ||
		!canName(t, "owner", controllers.GatewayNamespace, "patch", "", "secrets", "", controllers.OIDCSecret) ||
		canName(t, "developer", controllers.GatewayNamespace, "patch", "", "secrets", "", controllers.OIDCSecret) {
		t.Error("RBAC on the client secret")
	}
	// Saving again without a secret keeps it.
	delete(body, "clientSecret")
	body["displayName"] = "Corp SSO"
	if code := c.owner.do(t, "PUT", "/api/v1/settings/sso", body, &out); code != http.StatusOK {
		t.Fatalf("save without secret: %d %v", code, out)
	}
	// The sign-in page sees it (through the cache, with the secret read by
	// the console's identity).
	eventually(t, func() error {
		var st map[string]any
		c.viewer.do(t, "GET", "/api/v1/sso", nil, &st)
		if st["enabled"] != true || st["displayName"] != "Corp SSO" {
			return fmt.Errorf("status %v", st)
		}
		return nil
	})
	if a := lastAudit(t, c.store, "settings.sso_secret"); a == nil || a.Actor != "owner@example.com" {
		t.Errorf("audit %+v", a)
	}
}

// kubeconfigFor downloads a kubeconfig as the session's user.
func kubeconfigFor(t *testing.T, s *session, body map[string]any) kubernetes.Interface {
	t.Helper()
	var out map[string]any
	if code := s.do(t, "POST", "/api/v1/account/kubeconfig", body, &out); code != http.StatusCreated {
		t.Fatalf("kubeconfig: %d %v", code, out)
	}
	kc, err := clientcmd.Load([]byte(out["kubeconfig"].(string)))
	if err != nil {
		t.Fatal(err)
	}
	cur := kc.Contexts[kc.CurrentContext]
	if cur == nil || kc.Clusters[cur.Cluster] == nil || kc.AuthInfos[cur.AuthInfo] == nil {
		t.Fatalf("kubeconfig without a usable current context: %+v", kc)
	}
	// clientcmd sends tokens only to https servers; the test console is
	// plain HTTP, so the config is assembled from the same fields.
	cfg := &rest.Config{Host: kc.Clusters[cur.Cluster].Server, BearerToken: kc.AuthInfos[cur.AuthInfo].Token}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestKubectlThroughTheProxy(t *testing.T) {
	c := newConsole(t)
	c.project(t, "kp-shop")
	c.project(t, "kp-blog")
	ctx := context.Background()

	// A viewer reads pods in a project, as kubectl would, and is who the
	// API server thinks.
	viewer := kubeconfigFor(t, c.viewer, map[string]any{})
	if _, err := viewer.CoreV1().Pods("kp-shop").List(ctx, metav1.ListOptions{}); err != nil {
		t.Fatalf("viewer lists pods: %v", err)
	}
	review, err := viewer.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if u := review.Status.UserInfo; u.Username != "kwerft:viewer@example.com" || !slices.Contains(u.Groups, "kwerft:role:viewer") ||
		slices.Contains(u.Groups, "system:masters") {
		t.Errorf("identity through the proxy: %+v", u)
	}
	// Kubernetes RBAC still decides: a viewer cannot write.
	_, err = viewer.CoreV1().ConfigMaps("kp-shop").Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x"}}, metav1.CreateOptions{})
	if !apierrors.IsForbidden(err) {
		t.Errorf("viewer writes: %v", err)
	}
	// Secrets are refused by the proxy, whatever the role.
	full := kubeconfigFor(t, c.owner, map[string]any{})
	if _, err := full.CoreV1().Secrets(controllers.GatewayNamespace).List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) ||
		!strings.Contains(err.Error(), "Kwerft") {
		t.Errorf("owner lists secrets: %v", err)
	}
	// An owner's token capped to viewer is a viewer to Kubernetes.
	capped := kubeconfigFor(t, c.owner, map[string]any{"role": "viewer"})
	err = capped.CoreV1().RESTClient().Post().AbsPath("/apis/kwerft.dev/v1alpha1/projects").
		Body([]byte(`{"apiVersion":"kwerft.dev/v1alpha1","kind":"Project","metadata":{"name":"kp-new"}}`)).Do(ctx).Error()
	if !apierrors.IsForbidden(err) {
		t.Errorf("viewer-capped owner creates a project: %v", err)
	}
	// A developer token limited to one project.
	dev := kubeconfigFor(t, c.dev, map[string]any{"projects": []string{"kp-shop"}})
	if _, err := dev.CoreV1().Pods("kp-shop").List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("own project: %v", err)
	}
	for name, err := range map[string]error{
		"other project": func() error { _, err := dev.CoreV1().Pods("kp-blog").List(ctx, metav1.ListOptions{}); return err }(),
		"all namespaces": func() error {
			_, err := dev.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
			return err
		}(),
		"kube-system": func() error {
			_, err := dev.CoreV1().ConfigMaps("kube-system").List(ctx, metav1.ListOptions{})
			return err
		}(),
	} {
		if !apierrors.IsForbidden(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// kubectl --as is refused, not silently ignored.
	req, _ := http.NewRequest("GET", c.dev.url+"/k8s/api/v1/namespaces/kp-shop/pods", nil)
	var out map[string]any
	c.dev.do(t, "POST", "/api/v1/account/tokens", map[string]any{"name": "as"}, &out)
	req.Header.Set("Authorization", "Bearer "+out["token"].(string))
	req.Header.Set("Impersonate-User", "owner@example.com")
	req.Header.Add("Impersonate-Group", "system:masters")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden || !strings.Contains(string(b), "impersonat") {
		t.Errorf("--as: %d %s", res.StatusCode, b)
	}
}

func TestDataKeyRotationWritesTheSecret(t *testing.T) {
	requireCluster(t)
	ctx := context.Background()
	key := auth.NewDataKey()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: "kwerft-data-key"},
		Data: map[string][]byte{DataKeySecretKey: []byte(key)}}
	if err := cluster.admin.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), sec) })
	raw, _ := auth.ParseDataKey(key)
	console, err := client.New(cluster.console, client.Options{Scheme: controllers.NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	c := newConsole(t, func(cfg *Config) {
		cfg.DataKey = raw
		cfg.System = console // the console's own identity, as in production
		cfg.DataKeySecret.Namespace, cfg.DataKeySecret.Name = controllers.GatewayNamespace, "kwerft-data-key"
	})
	var out map[string]any
	if code := c.owner.do(t, "POST", "/api/v1/settings/data-key/rotate", map[string]string{"password": "a long test password"}, &out); code != http.StatusOK {
		t.Fatalf("rotate: %d %v", code, out)
	}
	if err := cluster.admin.Get(ctx, client.ObjectKeyFromObject(sec), sec); err != nil {
		t.Fatal(err)
	}
	next, err := auth.ParseDataKey(string(sec.Data[DataKeySecretKey]))
	if err != nil || auth.KeyID(next) != out["keyId"] || out["keyId"] == auth.KeyID(raw) {
		t.Errorf("secret key %v, response %v", err, out)
	}
	if _, ok := sec.Data[DataKeySecretPrevious]; ok {
		t.Error("previous key not retired")
	}
	// Owners themselves may not touch the data key's Secret.
	if canName(t, "owner", controllers.GatewayNamespace, "patch", "", "secrets", "", "kwerft-data-key") {
		t.Error("owners may patch the data key")
	}
}
