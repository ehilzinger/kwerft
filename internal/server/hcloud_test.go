// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

// Settings › Hetzner Cloud API against the test cluster with the chart's
// RBAC and the fake Cloud API.

const testHCloudToken = "hcloudTestToken0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLM"

func hcloudFixture(t *testing.T) *hetznertest.Server {
	t.Helper()
	settingsFixture(t)
	ctx := context.Background()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: controllers.HCloudTokenSecret}}
	if err := cluster.admin.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), sec) })
	hz := hetznertest.New(t, testHCloudToken)
	hz.EnableFirewalls()
	hz.AddServer(hetznertest.NewServerSummary(1, "kwerft-1", "fsn1", "203.0.113.24", "2001:db8:1::/64", 0, "", nil))
	hz.AddServer(hetznertest.NewServerSummary(2, "other", "hel1", "203.0.113.25", "2001:db8:2::/64", 0, "", nil))
	return hz
}

func withHCloud(hz *hetznertest.Server, more ...func(*Config)) func(*Config) {
	return func(c *Config) {
		withSettings(func(s *settingsAPI) { s.hetznerAPI = hz.URL })(c)
		for _, m := range more {
			m(c)
		}
	}
}

func storedHCloudToken(t *testing.T) string {
	t.Helper()
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.HCloudTokenSecret}, &sec); err != nil {
		t.Fatal(err)
	}
	return string(sec.Data[controllers.HCloudTokenKey])
}

func TestHCloudTokenIsCheckedAndWriteOnly(t *testing.T) {
	hz := hcloudFixture(t)
	c := newConsole(t, withHCloud(hz))

	// Developers and viewers cannot set it.
	for _, s := range []*session{c.dev, c.viewer} {
		if code := s.do(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": testHCloudToken}, nil); code != http.StatusForbidden {
			t.Errorf("non-admin sets the token: %d", code)
		}
	}
	var bad apiError
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": "not a token"}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("malformed: %d %+v", code, bad)
	}
	wrong := strings.Repeat("x", 64)
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": wrong}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("rejected: %d %+v", code, bad)
	}
	hz.SetReadOnly(true)
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": testHCloudToken}, &bad); code != http.StatusBadRequest ||
		bad.Field != "token" || !strings.Contains(bad.Error, "only read") {
		t.Errorf("read-only: %d %+v", code, bad)
	}
	if storedHCloudToken(t) != "" {
		t.Fatal("a refused token was stored")
	}
	hz.SetReadOnly(false)

	res := c.owner.raw(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": testHCloudToken})
	if res.code != http.StatusOK || strings.Contains(res.body, testHCloudToken) {
		t.Fatalf("save: %d %s", res.code, res.body)
	}
	if !strings.Contains(res.body, `"servers":2`) || !strings.Contains(res.body, `"locations":["fsn1","hel1"]`) {
		t.Errorf("check: %s", res.body)
	}
	if storedHCloudToken(t) != testHCloudToken {
		t.Error("token not stored")
	}
	if n := len(hz.Firewalls()); n != 0 {
		t.Errorf("checking the token left %d firewalls", n)
	}
	var view settingsJSON
	if code := c.viewer.do(t, "GET", "/api/v1/settings", nil, &view); code != http.StatusOK || !view.HCloud.TokenSet || view.HCloud.Firewall != "sync" {
		t.Errorf("view: %d %+v", code, view.HCloud)
	}
	for _, s := range []*session{c.owner, c.dev, c.viewer} {
		if got := s.raw(t, "GET", "/api/v1/settings", nil); strings.Contains(got.body, testHCloudToken) {
			t.Error("GET /settings returns the token")
		}
	}
	entries, err := c.store.RecentAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Target+e.Detail, testHCloudToken) {
			t.Errorf("audit entry carries the token: %+v", e)
		}
		found = found || e.Action == "settings.hcloud_token"
	}
	if !found {
		t.Error("not audited")
	}

	// Removing it empties the Secret and the marker.
	if code := c.owner.do(t, "DELETE", "/api/v1/settings/hcloud-token", nil, &view); code != http.StatusOK {
		t.Fatalf("remove: %d", code)
	}
	if storedHCloudToken(t) != "" || settingsNow(t).Annotations[controllers.AnnotationHCloudTokenUpdated] != "" {
		t.Error("token not removed")
	}

	// Kubernetes agrees: owners and admins patch the Secret, nobody reads it.
	for _, check := range []struct {
		role, verb string
		want       bool
	}{{"owner", "patch", true}, {"admin", "patch", true}, {"owner", "get", false}, {"admin", "get", false}, {"developer", "patch", false}, {"viewer", "patch", false}} {
		if got := canName(t, check.role, controllers.GatewayNamespace, check.verb, "", "secrets", "", controllers.HCloudTokenSecret); got != check.want {
			t.Errorf("%s may %s the hcloud token: %v, want %v", check.role, check.verb, got, check.want)
		}
	}
}

func TestHCloudSettings(t *testing.T) {
	hz := hcloudFixture(t)
	c := newConsole(t, withHCloud(hz))
	var bad apiError
	// No Load Balancer without a token.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud", map[string]any{"loadBalancer": map[string]any{"enabled": true}}, &bad); code != http.StatusBadRequest || bad.Field != "loadBalancer.enabled" {
		t.Errorf("without token: %d %+v", code, bad)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud-token", map[string]string{"token": testHCloudToken}, nil); code != http.StatusOK {
		t.Fatalf("token: %d", code)
	}
	// Nor without the ingress accepting the PROXY protocol.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud", map[string]any{"loadBalancer": map[string]any{"enabled": true}}, &bad); code != http.StatusBadRequest ||
		!strings.Contains(bad.Error, "installer") {
		t.Errorf("without proxy network: %d %+v", code, bad)
	}
	if code := c.dev.do(t, "PUT", "/api/v1/settings/hcloud", map[string]any{"firewall": "off"}, nil); code != http.StatusForbidden {
		t.Errorf("developer: %d", code)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud", map[string]any{"firewall": "maybe"}, &bad); code != http.StatusBadRequest || bad.Field != "firewall" {
		t.Errorf("bad mode: %d %+v", code, bad)
	}
	// The token is the management cluster's; the rest is per cluster (only
	// local is reachable without the multi-cluster console).
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud-token?cluster=second", map[string]string{"token": testHCloudToken}, &bad); code != http.StatusBadRequest || !strings.Contains(bad.Error, "management cluster") {
		t.Errorf("token for another cluster: %d %+v", code, bad)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud?cluster=second", map[string]any{"firewall": "off"}, nil); code != http.StatusNotFound {
		t.Errorf("unknown cluster: %d", code)
	}
	// A remote Cloud cluster's settings go to its Cluster object (the Cluster
	// reconciler hands them over); an adopted cluster has none here.
	ctx := context.Background()
	cloudy := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "cloudy"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterHetznerCloud,
		HetznerCloud: &kwerftv1.HetznerClusterSpec{Location: "fsn1", ServerType: "cx23", ControlPlanes: 1}}}
	adopted := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "adopted-1"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	for _, cl := range []*kwerftv1.Cluster{cloudy, adopted} {
		if err := cluster.admin.Create(ctx, cl); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), cl) })
	}
	var cloudView struct {
		Cloud clusterCloudJSON `json:"cloud"`
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud?cluster=cloudy", map[string]any{"firewall": "off", "loadBalancer": map[string]any{"enabled": true, "type": "LB11"}}, &cloudView); code != http.StatusOK ||
		cloudView.Cloud.Firewall != "off" || !cloudView.Cloud.LoadBalancer.Enabled || cloudView.Cloud.LoadBalancer.Type != "lb11" {
		t.Errorf("cloud cluster: %d %+v", code, cloudView)
	}
	var got kwerftv1.Cluster
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "cloudy"}, &got); err != nil || got.Spec.HetznerCloud.Firewall != kwerftv1.CloudFirewallOff ||
		got.Spec.HetznerCloud.LoadBalancer == nil || !got.Spec.HetznerCloud.LoadBalancer.Enabled || got.Spec.HetznerCloud.ServerType != "cx23" {
		t.Errorf("cluster spec %+v %v", got.Spec.HetznerCloud, err)
	}
	if code := c.dev.do(t, "PUT", "/api/v1/settings/hcloud?cluster=cloudy", map[string]any{"firewall": "sync"}, nil); code != http.StatusForbidden {
		t.Errorf("developer on a cluster: %d", code)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud?cluster=adopted-1", map[string]any{"firewall": "off"}, &bad); code != http.StatusBadRequest || !strings.Contains(bad.Error, "Hetzner Cloud clusters") {
		t.Errorf("adopted cluster: %d %+v", code, bad)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud?cluster=a.b", map[string]any{"firewall": "off"}, &bad); code != http.StatusBadRequest || bad.Field != "cluster" {
		t.Errorf("bad cluster name: %d %+v", code, bad)
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/hcloud?cluster=local", map[string]any{"firewall": "off"}, nil); code != http.StatusOK {
		t.Fatalf("off: %d", code)
	}
	if h := settingsNow(t).Spec.HetznerCloud; h == nil || h.Firewall != kwerftv1.CloudFirewallOff || h.LoadBalancer == nil || h.LoadBalancer.Enabled {
		t.Errorf("spec %+v", h)
	}

	c2 := newConsole(t, withHCloud(hz, func(cfg *Config) { cfg.HCloudProxyNetwork = "10.0.0.0/16" }))
	var view settingsJSON
	if code := c2.owner.do(t, "PUT", "/api/v1/settings/hcloud", map[string]any{"firewall": "sync", "loadBalancer": map[string]any{"enabled": true, "type": "LB21", "location": "nbg1"}}, &view); code != http.StatusOK {
		t.Fatalf("lb: %d", code)
	}
	if h := settingsNow(t).Spec.HetznerCloud; h.Firewall != kwerftv1.CloudFirewallSync || !h.LoadBalancer.Enabled || h.LoadBalancer.Type != "lb21" || h.LoadBalancer.Location != "nbg1" {
		t.Errorf("spec %+v %+v", h, h.LoadBalancer)
	}
	if !view.HCloud.LoadBalancerReady || !view.HCloud.LoadBalancer.Enabled {
		t.Errorf("view %+v", view.HCloud)
	}
	// What the reconciler reports is shown.
	cs := settingsNow(t)
	cs.Status.HetznerCloud = &kwerftv1.HetznerCloudStatus{Firewall: &kwerftv1.CloudFirewallStatus{State: "InSync", Rules: 5, Servers: 1},
		LoadBalancer: &kwerftv1.LoadBalancerStatus{State: "Active", Active: true, IPv4: "198.51.100.7"}}
	if err := cluster.admin.Status().Update(context.Background(), cs); err != nil {
		t.Fatal(err)
	}
	if code := c2.viewer.do(t, "GET", "/api/v1/settings", nil, &view); code != http.StatusOK || view.HCloud.Status == nil ||
		view.HCloud.Status.Firewall.State != "InSync" || view.HCloud.Status.LoadBalancer.IPv4 != "198.51.100.7" {
		t.Errorf("status view %+v", view.HCloud.Status)
	}
}

func TestVolumeClasses(t *testing.T) {
	hz := hcloudFixture(t)
	exists := false
	c := newConsole(t, withHCloud(hz, func(cfg *Config) {
		cfg.StorageClassExists = func(_ context.Context, name string) bool { return exists && name == HCloudVolumesClass }
	}))
	var classes []volumeClassJSON
	if code := c.viewer.do(t, "GET", "/api/v1/volume-classes", nil, &classes); code != http.StatusOK || len(classes) != 2 ||
		!classes[0].Available || classes[1].Available || classes[1].Reason == "" {
		t.Errorf("without CSI: %d %+v", code, classes)
	}
	var bad apiError
	if code := c.dev.do(t, "POST", "/api/v1/projects/nocsi/volumes", map[string]string{"name": "data", "size": "1Gi", "class": "hcloud-volume"}, &bad); code != http.StatusUnprocessableEntity || bad.Field != "class" {
		t.Errorf("create without CSI: %d %+v", code, bad)
	}
	exists = true
	if code := c.viewer.do(t, "GET", "/api/v1/volume-classes", nil, &classes); code != http.StatusOK || !classes[1].Available {
		t.Errorf("with CSI: %d %+v", code, classes)
	}
}
