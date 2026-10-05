package server

import (
	"context"
	"net/http"
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// TestMultiClusterDNS: the records the console's DNS reconciler keeps for a
// remote cluster's hostnames (Cluster status.dns) show next to its Domains
// and in that cluster's settings, and a remote cluster cannot take the
// console's apps domain's wildcard or records.
func TestMultiClusterDNS(t *testing.T) {
	e := newMultiConsole(t, withSettings(), func(c *Config) { c.System = cluster.admin })
	ctx := context.Background()
	settingsFixture(t)
	cs := settingsNow(t)
	cs.Spec = kwerftv1.ConsoleSettingsSpec{AppsDomain: "apps.example.com", TLS: kwerftv1.TLSHTTP01,
		DNS: &kwerftv1.DNSSettings{Provider: "hetzner", ManageRecords: true}}
	if err := cluster.admin.Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	e.projectIn(t, "dns-edge", "edge")
	d := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "dns-edge", Name: "router"}, Spec: kwerftv1.DomainSpec{Hostname: "router.apps.example.com"}}
	if err := e.edge.admin.Create(ctx, d); err != nil {
		t.Fatal(err)
	}

	// What the console's reconcilers report on the Cluster.
	cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	if err := cluster.admin.Create(ctx, cl); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(context.Background(), cl) })
	cl.Status = kwerftv1.ClusterStatus{PublicAddresses: []string{"198.51.100.7"},
		Hostnames: []kwerftv1.ClusterHostname{{Hostname: "router.apps.example.com", Project: "dns-edge"}},
		DNS: &kwerftv1.DNSStatus{Zones: []string{"example.com"}, Records: []kwerftv1.DNSRecordStatus{{Hostname: "router.apps.example.com",
			Purpose: "app", Project: "dns-edge", Zone: "example.com", State: kwerftv1.DNSManaged, Values: []string{"198.51.100.7"}}}}}
	if err := cluster.admin.Status().Update(ctx, cl); err != nil {
		t.Fatal(err)
	}

	var domains []domainJSON
	if code := e.owner.do(t, "GET", "/api/v1/domains", nil, &domains); code != http.StatusOK {
		t.Fatalf("domains: %d", code)
	}
	i := slices.IndexFunc(domains, func(d domainJSON) bool { return d.Hostname == "router.apps.example.com" })
	if i < 0 || domains[i].Cluster != "edge" || domains[i].DNS == nil || domains[i].DNS.State != "Managed" ||
		!slices.Equal(domains[i].DNS.Values, []string{"198.51.100.7"}) {
		t.Errorf("domains %+v", domains)
	}

	var view settingsJSON
	if code := e.owner.do(t, "GET", "/api/v1/settings?cluster=edge", nil, &view); code != http.StatusOK {
		t.Fatalf("settings: %d", code)
	}
	if view.Cluster != "edge" || view.ConsoleAppsDomain != "apps.example.com" || !view.ConsoleRecords ||
		view.ClusterDNS == nil || len(view.ClusterDNS.Records) != 1 || view.ClusterDNS.Records[0].Project != "dns-edge" {
		t.Errorf("edge settings %+v %+v", view, view.ClusterDNS)
	}
	// The console's own settings say nothing of it.
	var local settingsJSON
	if code := e.owner.do(t, "GET", "/api/v1/settings", nil, &local); code != http.StatusOK || local.ConsoleAppsDomain != "" || local.ClusterDNS != nil {
		t.Errorf("local settings %d %+v", code, local)
	}

	var bad apiError
	if code := e.owner.do(t, "PUT", "/api/v1/settings/apps?cluster=edge", map[string]any{"appsDomain": "apps.example.com", "tls": "dns01"}, &bad); code != http.StatusBadRequest || bad.Field != "tls" {
		t.Errorf("dns01 under the console's apps domain: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "PUT", "/api/v1/settings/apps?cluster=edge", map[string]any{"appsDomain": "apps.example.com", "manageRecords": true}, &bad); code != http.StatusBadRequest || bad.Field != "manageRecords" {
		t.Errorf("records under the console's apps domain: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "PUT", "/api/v1/settings/apps?cluster=edge", map[string]any{"appsDomain": "apps.example.com", "tls": "http01"}, nil); code != http.StatusOK {
		t.Errorf("http01 under the console's apps domain: %d", code)
	}
	t.Cleanup(func() {
		_ = e.edge.admin.Delete(context.Background(), &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}})
	})
}
