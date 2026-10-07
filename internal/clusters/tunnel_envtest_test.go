// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/kube"
)

// agentUser stands in for the agent's service account in the remote
// cluster; it gets the chart's kwerft-controller ClusterRole, as the real
// one does.
const agentUser = "system:serviceaccount:kwerft-system:kwerft"

// observabilityNamespace is the chart's observability.namespace.
const observabilityNamespace = "kwerft-observability"

func startEnv(t *testing.T) (*envtest.Environment, *rest.Config) {
	t.Helper()
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "charts", "kwerft", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	return env, cfg
}

func testScheme(t *testing.T) *runtime.Scheme {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := kwerftv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// applyChartRoles applies the chart's ClusterRoles and ClusterRoleBindings
// (template lines dropped), so the remote cluster has the console's roles
// and the agent's own ClusterRole exactly as installed.
func applyChartRoles(ctx context.Context, c client.Client) error {
	for _, file := range []string{"roles.yaml", "rbac.yaml"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kwerft", "templates", file))
		if err != nil {
			return err
		}
		var kept []string
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.Contains(line, "{{") {
				kept = append(kept, line)
			}
		}
		for _, doc := range strings.Split(strings.Join(kept, "\n"), "\n---") {
			var head metav1.TypeMeta
			if err := yaml.Unmarshal([]byte(doc), &head); err != nil {
				return err
			}
			var obj client.Object
			switch {
			case head.Kind == "ClusterRole":
				obj = &rbacv1.ClusterRole{}
			case head.Kind == "ClusterRoleBinding" && file == "roles.yaml":
				obj = &rbacv1.ClusterRoleBinding{}
			// The Kwerft service account's Roles (rbac.yaml): their
			// namespace and subject lines are template lines, so fill
			// them in for the agent's stand-in.
			case head.Kind == "Role" && file == "rbac.yaml":
				obj = &rbacv1.Role{}
			case head.Kind == "RoleBinding" && file == "rbac.yaml":
				obj = &rbacv1.RoleBinding{}
			default:
				continue
			}
			if err := yaml.UnmarshalStrict([]byte(doc), obj); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
			switch o := obj.(type) {
			case *rbacv1.Role:
				o.Namespace = observabilityNamespace
			case *rbacv1.RoleBinding:
				o.Namespace = observabilityNamespace
				o.Subjects = []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: agentUser}}
			}
			if ns := obj.GetNamespace(); ns != "" {
				if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil && !apierrors.IsAlreadyExists(err) {
					return err
				}
			}
			if err := c.Create(ctx, obj); err != nil {
				return fmt.Errorf("%s: %w", file, err)
			}
		}
	}
	return c.Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "kwerft-controller"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "kwerft-controller"},
		Subjects:   []rbacv1.Subject{{APIGroup: rbacv1.GroupName, Kind: "User", Name: agentUser}},
	})
}

// TestTunnelToARemoteAPIServer: a management and a "remote" API server, an
// agent in the remote one connected to a console test server, and requests
// through the Registry's rest.Config that reach the remote API with the
// agent's identity and, for users, the console's impersonation.
func TestTunnelToARemoteAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set (run `make test`)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	scheme := testScheme(t)

	_, mgmtCfg := startEnv(t)
	mgmt, err := client.New(mgmtCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	remoteEnv, remoteCfg := startEnv(t)
	remote, err := client.New(remoteCfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	if err := applyChartRoles(ctx, remote); err != nil {
		t.Fatal(err)
	}
	agentID, err := remoteEnv.AddUser(envtest.User{Name: agentUser}, nil)
	if err != nil {
		t.Fatal(err)
	}

	token := NewAgentToken("edge")
	if err := mgmt.Create(ctx, adoptedCluster("edge", token)); err != nil {
		t.Fatal(err)
	}
	hub := &Hub{Local: mgmtCfg, Clusters: mgmt, Logger: slog.New(slog.DiscardHandler)}
	console := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = hub.Connect(w, r) }))
	defer console.Close()
	pool := x509.NewCertPool()
	pool.AddCert(console.Certificate())
	agentCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	agent := &Agent{ConsoleURL: console.URL, Token: func() (string, error) { return token, nil }, API: agentID.Config(),
		Namespace: "kwerft-system", TLS: &tls.Config{RootCAs: pool}, Logger: slog.New(slog.DiscardHandler),
		MinBackoff: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond}
	go func() { _ = agent.Run(agentCtx) }()
	waitFor(t, "the agent", connected(hub, "edge"))

	st, _ := hub.Agent("edge")
	if !strings.HasPrefix(st.Info.KubernetesVersion, "v1.") || st.Info.Error != "" {
		t.Errorf("agent info = %+v", st.Info)
	}
	cfg, err := hub.RESTConfig("edge")
	if err != nil {
		t.Fatal(err)
	}

	// Kwerft's own identity there is the agent's service account.
	cs := kubernetes.NewForConfigOrDie(cfg)
	who, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if who.Status.UserInfo.Username != agentUser {
		t.Errorf("identity through the tunnel: %q, want %q", who.Status.UserInfo.Username, agentUser)
	}

	// What the console does there with Kwerft's own identity (W4): informer
	// caches of Kwerft's kinds and namespaces, Builds from Git webhooks,
	// metrics, logs and Alertmanager through the service proxy, and
	// impersonating users. Asked of the API server through the tunnel.
	type check struct{ group, resource, sub, verb, ns, name string }
	var checks []check
	for _, r := range []string{"projects", "apps", "tasks", "schedules", "volumes", "domains", "builds", "alertrules", "consolesettings", "firewallrules",
		"notificationchannels", "gitconnections", "trafficrules"} {
		checks = append(checks, check{group: "kwerft.dev", resource: r, verb: "list"}, check{group: "kwerft.dev", resource: r, verb: "watch"})
	}
	checks = append(checks,
		check{resource: "namespaces", verb: "list"}, check{resource: "namespaces", verb: "watch"},
		check{group: "kwerft.dev", resource: "builds", verb: "create", ns: "shop"},
		check{resource: "services", sub: "proxy", verb: "get", ns: observabilityNamespace},
		check{resource: "services", sub: "proxy", verb: "create", ns: observabilityNamespace},
		check{resource: "users", verb: "impersonate", name: "kwerft:dev@example.com"},
		check{resource: "groups", verb: "impersonate", name: "kwerft:role:developer"},
		check{resource: "groups", verb: "impersonate", name: "system:authenticated"},
	)
	for _, c := range checks {
		review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{
			Group: c.group, Resource: c.resource, Subresource: c.sub, Verb: c.verb, Namespace: c.ns, Name: c.name}}}
		got, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Status.Allowed {
			t.Errorf("the agent may not %s %s.%s/%s %s in %q", c.verb, c.resource, c.group, c.sub, c.name, c.ns)
		}
	}
	// ...but no other observability namespace's proxy, and no system:masters.
	for _, c := range []check{
		{resource: "services", sub: "proxy", verb: "create", ns: "kube-system"},
		{resource: "groups", verb: "impersonate", name: "system:masters"},
	} {
		review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{
			Group: c.group, Resource: c.resource, Subresource: c.sub, Verb: c.verb, Namespace: c.ns, Name: c.name}}}
		got, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Allowed {
			t.Errorf("the agent may %s %s/%s %s in %q", c.verb, c.resource, c.sub, c.name, c.ns)
		}
	}

	// Users act as themselves, exactly as in the local cluster: the console's
	// Impersonator works on the Registry's config unchanged.
	imp, err := kube.NewImpersonator(cfg, nil, nil, scheme)
	if err != nil {
		t.Fatal(err)
	}
	viewerCS, err := imp.Clientset("viewer@example.com", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	who, err = viewerCS.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if who.Status.UserInfo.Username != "kwerft:viewer@example.com" || !contains(who.Status.UserInfo.Groups, "kwerft:role:viewer") {
		t.Errorf("impersonated identity: %+v", who.Status.UserInfo)
	}
	if _, err := viewerCS.CoreV1().Secrets("default").List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("viewer lists secrets through the tunnel: %v, want forbidden", err)
	}
	viewer, err := imp.For("viewer@example.com", "viewer")
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.Create(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "nope"}}); !apierrors.IsForbidden(err) {
		t.Errorf("viewer creates a project: %v, want forbidden", err)
	}
	owner, err := imp.For("owner@example.com", "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Create(ctx, &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}); err != nil {
		t.Fatalf("owner creates a project through the tunnel: %v", err)
	}
	var p kwerftv1.Project
	if err := remote.Get(ctx, client.ObjectKey{Name: "shop"}, &p); err != nil {
		t.Errorf("the project is not in the remote cluster: %v", err)
	}
	if err := mgmt.Get(ctx, client.ObjectKey{Name: "shop"}, &p); !apierrors.IsNotFound(err) {
		t.Errorf("the project leaked into the management cluster: %v", err)
	}

	// The agent can impersonate only the console's groups, never system:masters.
	masters := rest.CopyConfig(cfg)
	masters.Impersonate = rest.ImpersonationConfig{UserName: "kwerft:x@example.com", Groups: []string{"system:masters"}}
	if _, err := kubernetes.NewForConfigOrDie(masters).CoreV1().Secrets("default").List(ctx, metav1.ListOptions{}); !apierrors.IsForbidden(err) {
		t.Errorf("impersonating system:masters through the tunnel: %v, want forbidden", err)
	}

	// Watches stream through the tunnel.
	if err := remote.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "demo"}}); err != nil {
		t.Fatal(err)
	}
	w, err := cs.CoreV1().ConfigMaps("demo").Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	for i := range 3 {
		name := fmt.Sprintf("cm-%d", i)
		if err := remote.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"}}); err != nil {
			t.Fatal(err)
		}
		select {
		case ev := <-w.ResultChan():
			if cm, ok := ev.Object.(*corev1.ConfigMap); ev.Type != watch.Added || !ok || cm.Name != name {
				t.Errorf("watch event %d: %s %v", i, ev.Type, ev.Object)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("no watch event for %s", name)
		}
	}

	// The agent goes away: the cluster is unavailable, the watch ends, and
	// Changed says so.
	changed := hub.Changed()
	stopAgent()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("Changed not closed when the agent left")
	}
	if _, err := hub.RESTConfig("edge"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("RESTConfig after the agent left: %v", err)
	}
	select {
	case _, open := <-w.ResultChan():
		if open {
			// A last event may still be buffered; the channel must close next.
			if _, open = <-w.ResultChan(); open {
				t.Error("watch still open after the agent left")
			}
		}
	case <-time.After(10 * time.Second):
		t.Error("watch did not end when the agent left")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
