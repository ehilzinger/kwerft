package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
	"github.com/ehilzinger/kwerft/internal/jointoken"
)

var nodesDataKey = []byte("nodes test data key, 32 bytes!!!")

// twoClusters is a Registry with "local" and "remote", both the test API
// server reached with the console's own identity.
type twoClusters struct{ cfg *rest.Config }

func (t twoClusters) List(context.Context) ([]clusters.Info, error) {
	return []clusters.Info{{Name: clusters.Local, Connected: true}, {Name: "remote", Connected: true}}, nil
}

func (t twoClusters) RESTConfig(name string) (*rest.Config, error) {
	switch name {
	case clusters.Local, "remote":
		return rest.CopyConfig(t.cfg), nil
	case "away":
		return nil, clusters.ErrUnavailable
	}
	return nil, clusters.ErrUnknown
}

func (t twoClusters) Changed() <-chan struct{} { return make(chan struct{}) }

type nodesEnv struct {
	c     *console
	cloud *hetznertest.Server
	robot *hetznertest.Robot
	sys   client.Client
}

func newNodesConsole(t *testing.T) *nodesEnv {
	t.Helper()
	requireCluster(t)
	sys, err := client.New(cluster.console, client.Options{Scheme: cluster.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	e := &nodesEnv{sys: sys, cloud: hetznertest.New(t, "cloud-token"), robot: hetznertest.NewRobot(t, "robot", "robot-pw")}
	e.cloud.FakeNodes()
	e.cloud.FakeClusterNetworks(hetzner.NetworkRef{Name: hetzner.NetworkName("local"), IPRange: "10.0.0.0/16"})
	e.c = newConsole(t, func(cfg *Config) {
		cfg.System, cfg.SystemReader = sys, sys
		cfg.DataKey = nodesDataKey
		cfg.ConsoleDomain = "console.example.com"
		cfg.Clusters = twoClusters{cfg: cluster.console}
		cfg.nodesHook = func(n *nodesAPI) {
			n.hcloud = func(context.Context) (*hetzner.Client, error) { return e.cloud.Client(), nil }
			n.robotBase = e.robot.URL
			n.consoleURL = func(*http.Request) string { return "https://console.example.com" }
		}
	})
	return e
}

func testNode(t *testing.T, name string, labels map[string]string) {
	t.Helper()
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if err := cluster.admin.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.Now()}}
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.5"}}
	if err := cluster.admin.Status().Update(ctx, n); err != nil {
		t.Fatal(err)
	}
}

func joinSecret(t *testing.T, name string) {
	t.Helper()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: controllers.JoinSecretName(name), Namespace: controllers.GatewayNamespace},
		StringData: map[string]string{"server": "https://10.0.0.2:6443", "token": "K10cafe::server:k3s-server-secret"}}
	if err := cluster.admin.Create(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), sec) })
}

func TestNodePoolsAPI(t *testing.T) {
	e := newNodesConsole(t)
	c := e.c
	ctx := context.Background()
	testNode(t, "nodes-cp-1", map[string]string{controllers.LabelControlPlane: "true", "kwerft.dev/platform": "cloud"})
	testNode(t, "nodes-worker-1", nil)

	// Owners and admins only.
	for _, s := range []*session{c.dev, c.viewer} {
		if code := s.do(t, "GET", "/api/v1/clusters/local/nodes", nil, nil); code != http.StatusForbidden {
			t.Fatalf("%d for a developer or viewer", code)
		}
		if code := s.do(t, "POST", "/api/v1/clusters/local/join-command", map[string]any{}, nil); code != http.StatusForbidden {
			t.Fatalf("join command: %d", code)
		}
	}

	var list nodesJSON
	if code := c.owner.do(t, "GET", "/api/v1/clusters/local/nodes", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if !list.Reachable || list.ControlPlanes != 1 || list.Joinable || len(list.Nodes) < 2 {
		t.Fatalf("list = %+v", list)
	}
	if code := c.owner.do(t, "GET", "/api/v1/clusters/nope/nodes", nil, nil); code != http.StatusNotFound {
		t.Fatalf("unknown cluster: %d", code)
	}

	// Creating pools: validation, then a worker pool.
	var apiErr apiError
	for _, bad := range []map[string]any{
		{"name": "Bad_Name", "serverType": "cx23", "location": "fsn1", "count": 1},
		{"name": "w", "serverType": "cx99", "location": "fsn1", "count": 1},
		{"name": "w", "serverType": "cx23", "location": "fsn1", "count": 51},
		{"name": "w", "serverType": "cx23", "location": "fsn1", "count": 1, "labels": map[string]string{"kubernetes.io/role": "x"}},
		// One control-plane node exists: one more makes two.
		{"name": "cp", "role": "control-plane", "serverType": "cx23", "location": "fsn1", "count": 1},
	} {
		if code := c.owner.do(t, "POST", "/api/v1/clusters/local/pools", bad, &apiErr); code != http.StatusBadRequest {
			t.Fatalf("%v: %d %+v", bad, code, apiErr)
		}
	}
	var pool poolJSON
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/pools", map[string]any{"name": "web", "serverType": "cx23", "location": "fsn1", "count": 2,
		"labels": map[string]string{"tier": "web"}}, &pool); code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "local-web"}})
	})
	if pool.Name != "local-web" || pool.Pool != "web" || pool.Role != "worker" {
		t.Fatalf("pool = %+v", pool)
	}
	var np kwerftv1.NodePool
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "local-web"}, &np); err != nil || np.Spec.Cluster != "local" || np.Spec.Count != 2 {
		t.Fatalf("pool object = %+v, %v", np.Spec, err)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/pools", map[string]any{"name": "cp", "role": "control-plane", "serverType": "cx23",
		"location": "fsn1", "count": 2}, nil); code != http.StatusCreated {
		t.Fatalf("control-plane pool of two (three in all): %d", code)
	}
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "local-cp"}})
	})
	if code := c.owner.do(t, "PATCH", "/api/v1/clusters/local/pools/local-cp", map[string]any{"count": 1}, &apiErr); code != http.StatusBadRequest {
		t.Fatalf("control plane to two: %d", code)
	}

	// Scale, change type; name and role stay.
	if code := c.owner.do(t, "PATCH", "/api/v1/clusters/local/pools/local-web", map[string]any{"count": 3, "serverType": "cx33"}, &pool); code != http.StatusOK || pool.Count != 3 || pool.ServerType != "cx33" {
		t.Fatalf("update: %d %+v", code, pool)
	}
	if code := c.owner.do(t, "PATCH", "/api/v1/clusters/local/pools/local-web", map[string]any{"role": "builds"}, nil); code != http.StatusBadRequest {
		t.Fatalf("role change: %d", code)
	}
	if code := c.owner.do(t, "PATCH", "/api/v1/clusters/remote/pools/local-web", map[string]any{"count": 1}, nil); code != http.StatusNotFound {
		t.Fatalf("pool of another cluster: %d", code)
	}

	// A server that never joined can be removed from its pool.
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "local-web"}, &np); err != nil {
		t.Fatal(err)
	}
	np.Status.Nodes = []kwerftv1.PoolNode{{Name: "local-web-abcde", Phase: "Failed"}}
	if err := cluster.admin.Status().Update(ctx, &np); err != nil {
		t.Fatal(err)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/pools/local-web/servers/local-web-abcde/remove", nil, nil); code != http.StatusAccepted {
		t.Fatalf("server remove: %d", code)
	}
	_ = cluster.admin.Get(ctx, client.ObjectKey{Name: "local-web"}, &np)
	if np.Annotations[controllers.AnnotationRemoveServers] != "local-web-abcde" {
		t.Fatalf("annotations = %v", np.Annotations)
	}

	if code := c.owner.do(t, "GET", "/api/v1/clusters/local/nodes", nil, &list); code != http.StatusOK || len(list.Pools) != 2 || list.Pools[1].Pool != "web" {
		t.Fatalf("list: %d %+v", code, list.Pools)
	}

	// Drain, uncordon, remove.
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/nodes/nodes-worker-1/drain", nil, nil); code != http.StatusAccepted {
		t.Fatalf("drain: %d", code)
	}
	var node corev1.Node
	_ = cluster.admin.Get(ctx, client.ObjectKey{Name: "nodes-worker-1"}, &node)
	if node.Annotations[controllers.AnnotationNodeDrain] != "true" {
		t.Fatalf("annotations = %v", node.Annotations)
	}
	node.Spec.Unschedulable = true // as the reconciler would
	_ = cluster.admin.Update(ctx, &node)
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/nodes/nodes-worker-1/uncordon", nil, nil); code != http.StatusOK {
		t.Fatalf("uncordon: %d", code)
	}
	_ = cluster.admin.Get(ctx, client.ObjectKey{Name: "nodes-worker-1"}, &node)
	if node.Spec.Unschedulable || node.Annotations[controllers.AnnotationNodeDrain] != "" {
		t.Fatalf("still cordoned: %+v", node.Annotations)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/nodes/nodes-cp-1/remove", nil, &apiErr); code != http.StatusConflict ||
		!strings.Contains(apiErr.Error, "last control-plane node") {
		t.Fatalf("last control plane: %d %+v", code, apiErr)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/nodes/nodes-worker-1/remove", map[string]any{"force": true}, nil); code != http.StatusAccepted {
		t.Fatalf("remove: %d", code)
	}
	_ = cluster.admin.Get(ctx, client.ObjectKey{Name: "nodes-worker-1"}, &node)
	if node.Annotations[controllers.AnnotationNodeRemove] != "force" {
		t.Fatalf("annotations = %v", node.Annotations)
	}

	// The remote cluster is read as the user through the Registry.
	remote := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "remote"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	if err := cluster.admin.Create(ctx, remote); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), remote) })
	if code := c.owner.do(t, "GET", "/api/v1/clusters/remote/nodes", nil, &list); code != http.StatusOK || !list.Reachable || list.Provider != "adopted" {
		t.Fatalf("remote: %d %+v", code, list)
	}
	away := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "away"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	if err := cluster.admin.Create(ctx, away); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), away) })
	if code := c.owner.do(t, "GET", "/api/v1/clusters/away/nodes", nil, &list); code != http.StatusOK || list.Reachable || list.Problem == "" {
		t.Fatalf("away: %d %+v", code, list)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/away/nodes/x/drain", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("drain in an unreachable cluster: %d", code)
	}

	// Deleting the pool.
	if code := c.owner.do(t, "DELETE", "/api/v1/clusters/local/pools/local-web", nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}

	entries, err := c.store.RecentAudit(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, a := range entries {
		seen[a.Action] = true
	}
	for _, a := range []string{"nodepool.create", "nodepool.update", "nodepool.delete", "nodepool.server_remove", "node.drain", "node.uncordon", "node.remove"} {
		if !seen[a] {
			t.Errorf("no %s in the audit log", a)
		}
	}
}

func TestJoinCommandAndJoin(t *testing.T) {
	e := newNodesConsole(t)
	c := e.c
	ctx := context.Background()

	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/join-command", map[string]any{"role": "worker"}, nil); code != http.StatusConflict {
		t.Fatalf("without join material: %d", code)
	}
	joinSecret(t, "local")
	var jc joinCommandJSON
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/join-command", map[string]any{"role": "worker", "ttlMinutes": 30}, &jc); code != http.StatusCreated {
		t.Fatalf("join command: %d", code)
	}
	if !strings.HasPrefix(jc.Command, "curl -fsSL https://console.example.com/join.sh | sudo bash -s -- --token kwft_join_") ||
		!strings.HasSuffix(jc.Command, "--role worker") || time.Until(jc.ExpiresAt) > 31*time.Minute {
		t.Fatalf("command = %+v", jc)
	}
	tok := strings.Fields(strings.SplitN(jc.Command, "--token ", 2)[1])[0]

	join := func(token, query string) (int, map[string]string) {
		t.Helper()
		req, _ := http.NewRequest("GET", c.owner.url+"/api/v1/join"+query, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out map[string]string
		raw, _ := io.ReadAll(res.Body)
		_ = json.Unmarshal(raw, &out)
		return res.StatusCode, out
	}
	if code, _ := join("", "?role=worker"); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := join("kwft_join_forged.sig", "?role=worker"); code != http.StatusUnauthorized {
		t.Fatalf("forged token: %d", code)
	}
	if code, _ := join(tok, "?role=control-plane"); code != http.StatusForbidden {
		t.Fatalf("role mismatch: %d", code)
	}
	code, out := join(tok, "?role=worker&node=dedi-1")
	if code != http.StatusOK || out["server"] != "https://10.0.0.2:6443" || !strings.HasPrefix(out["token"], "K10cafe::") || strings.Contains(out["token"], "server:") {
		t.Fatalf("join: %d %v", code, out)
	}
	// The worker got a bootstrap token that expires, not the server token.
	id := strings.SplitN(strings.TrimPrefix(out["token"], "K10cafe::"), ".", 2)[0]
	var bt corev1.Secret
	if err := cluster.admin.Get(ctx, client.ObjectKey{Namespace: "kube-system", Name: "bootstrap-token-" + id}, &bt); err != nil || bt.Type != corev1.SecretTypeBootstrapToken {
		t.Fatalf("bootstrap token: %v %s", err, bt.Type)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), &bt) })

	// Control-plane join commands: the server token.
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/join-command", map[string]any{"role": "control-plane"}, &jc); code != http.StatusCreated {
		t.Fatalf("control-plane join command: %d", code)
	}
	cpTok := strings.Fields(strings.SplitN(jc.Command, "--token ", 2)[1])[0]
	if code, out := join(cpTok, "?role=control-plane&node=cp-2"); code != http.StatusOK || out["token"] != "K10cafe::server:k3s-server-secret" {
		t.Fatalf("control-plane join: %d %v", code, out)
	}

	// Tokens bound to a server (cloud-init): only that host, only once.
	signer := jointoken.NewSigner(nodesDataKey)
	bound, _, _ := signer.Issue(jointoken.Claims{Cluster: "local", Role: jointoken.RoleWorker, Node: "local-web-xyz12"}, time.Hour)
	if code, _ := join(bound, "?role=worker&node=someone-else"); code != http.StatusForbidden {
		t.Fatalf("other host: %d", code)
	}
	testNode(t, "local-web-xyz12", nil)
	if code, _ := join(bound, "?role=worker&node=local-web-xyz12"); code != http.StatusConflict {
		t.Fatalf("spent token: %d", code)
	}
	signer.Now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	old, _, _ := signer.Issue(jointoken.Claims{Cluster: "local", Role: jointoken.RoleWorker}, time.Hour)
	if code, out := join(old, "?role=worker"); code != http.StatusUnauthorized || !strings.Contains(out["error"], "expired") {
		t.Fatalf("expired: %d %v", code, out)
	}

	// The scripts the join command runs.
	res, err := http.Get(c.owner.url + "/join.sh")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(raw), "https://console.example.com") || strings.Contains(string(raw), "__CONSOLE_URL__") {
		t.Fatalf("join.sh: %d", res.StatusCode)
	}
	res, err = http.Get(c.owner.url + "/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(string(raw), "#!/usr/bin/env bash") {
		t.Fatalf("install.sh: %d", res.StatusCode)
	}

	entries, _ := c.store.RecentAudit(ctx, 200)
	seen := map[string]int{}
	for _, a := range entries {
		seen[a.Action]++
	}
	if seen["cluster.join_command"] != 2 || seen["cluster.node_join"] != 2 || seen["cluster.node_join.denied"] != 1 {
		t.Fatalf("audit = %v", seen)
	}
}

func TestCatalogAndVSwitch(t *testing.T) {
	e := newNodesConsole(t)
	c := e.c
	var cat struct {
		Locations   []locationJSON   `json:"locations"`
		ServerTypes []serverTypeJSON `json:"serverTypes"`
	}
	if code := c.owner.do(t, "GET", "/api/v1/hetzner/catalog", nil, &cat); code != http.StatusOK || len(cat.Locations) != 4 || len(cat.ServerTypes) != 3 {
		t.Fatalf("catalog: %d %+v", code, cat)
	}
	if cat.ServerTypes[0].Prices["fsn1"] == "" {
		t.Fatalf("no prices: %+v", cat.ServerTypes[0])
	}
	if code := c.dev.do(t, "GET", "/api/v1/hetzner/catalog", nil, nil); code != http.StatusForbidden {
		t.Fatalf("developer: %d", code)
	}

	body := map[string]any{"robotUser": "robot", "robotPassword": "robot-pw", "vlan": 4000, "ipRange": "10.0.64.0/24", "servers": []string{"198.51.100.7"}}
	var apiErr apiError
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/vswitch", map[string]any{"robotUser": "robot", "robotPassword": "x", "vlan": 4000, "ipRange": "10.0.64.0/24"}, &apiErr); code != http.StatusBadRequest || apiErr.Field != "robotUser" {
		t.Fatalf("bad Robot credentials: %d %+v", code, apiErr)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/vswitch", map[string]any{"robotUser": "robot", "robotPassword": "robot-pw", "vlan": 4000, "ipRange": "192.168.1.0/24"}, &apiErr); code != http.StatusBadRequest || apiErr.Field != "ipRange" {
		t.Fatalf("range outside the network: %d %+v", code, apiErr)
	}
	var out map[string]any
	if code := c.owner.do(t, "POST", "/api/v1/clusters/local/vswitch", body, &out); code != http.StatusOK {
		t.Fatalf("vswitch: %d %v", code, out)
	}
	vs := e.robot.VSwitchesNow()
	if len(vs) != 1 || vs[0].VLAN != 4000 || len(vs[0].Server) != 1 || vs[0].Server[0].ServerIP != "198.51.100.7" {
		t.Fatalf("vswitches = %+v", vs)
	}
	if subs := e.cloud.VSwitchSubnets(); len(subs) != 1 || !strings.HasSuffix(subs[0], "/10.0.64.0/24/eu-central") {
		t.Fatalf("subnets = %v", subs)
	}
	// Owners only.
	if code := c.dev.do(t, "POST", "/api/v1/clusters/local/vswitch", body, nil); code != http.StatusForbidden {
		t.Fatalf("developer: %d", code)
	}
	entries, _ := c.store.RecentAudit(context.Background(), 50)
	for _, a := range entries {
		if a.Action == "cluster.vswitch" && strings.Contains(a.Detail, "robot-pw") {
			t.Fatal("the Robot password is in the audit log")
		}
	}
}
