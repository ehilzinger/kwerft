// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// Records for remote clusters' hostnames (dns_clusters.go): the Cluster
// reconciler's part of the Cluster status (addresses, hostnames) is set by
// hand.

var dnsEpoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func since(minutes int) metav1.Time {
	return metav1.Time{Time: dnsEpoch.Add(time.Duration(minutes) * time.Minute)}
}

// remoteCluster creates (or updates) a Cluster with the status the Cluster
// reconciler reports.
func (f *dnsFixture) remoteCluster(name string, addrs []string, hosts ...kwerftv1.ClusterHostname) {
	f.t.Helper()
	ctx := context.Background()
	var c kwerftv1.Cluster
	err := f.c.Get(ctx, client.ObjectKey{Name: name}, &c)
	if err != nil {
		c = kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
		if err := f.c.Create(ctx, &c); err != nil {
			f.t.Fatal(err)
		}
	}
	c.Status.PublicAddresses, c.Status.Hostnames = addrs, hosts
	if err := f.c.Status().Update(ctx, &c); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dnsFixture) cluster(name string) *kwerftv1.Cluster {
	f.t.Helper()
	var c kwerftv1.Cluster
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: name}, &c); err != nil {
		f.t.Fatal(err)
	}
	return &c
}

func (f *dnsFixture) clusterRecord(cluster, host string) kwerftv1.DNSRecordStatus {
	f.t.Helper()
	c := f.cluster(cluster)
	if c.Status.DNS == nil {
		f.t.Fatalf("cluster %s has no DNS status", cluster)
	}
	for _, r := range c.Status.DNS.Records {
		if r.Hostname == host {
			return r
		}
	}
	f.t.Fatalf("cluster %s: no DNS status for %s in %+v", cluster, host, c.Status.DNS.Records)
	return kwerftv1.DNSRecordStatus{}
}

// localDomain is a Domain of the console's cluster that won its hostname.
func (f *dnsFixture) localDomain(project, host string) {
	f.t.Helper()
	d := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: project, Name: "web"}, Spec: kwerftv1.DomainSpec{Hostname: host}}
	if err := f.c.Create(context.Background(), d); err != nil {
		f.t.Fatal(err)
	}
	d.Status.Listener = ListenerName(host)
	if err := f.c.Status().Update(context.Background(), d); err != nil {
		f.t.Fatal(err)
	}
}

func host(name, project string, at int) kwerftv1.ClusterHostname {
	return kwerftv1.ClusterHostname{Hostname: name, Project: project, Since: since(at)}
}

func TestDNSRecordsForRemoteClusters(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	f.rec.RemoteClusters = true
	f.localDomain("shop", "shop.apps.example.com")
	f.remoteCluster("edge", []string{"198.51.100.7", "2001:db8::7", "10.0.0.5"},
		host("router.apps.example.com", "maps", 0),
		host("shared.apps.example.com", "maps", 10),
		host("shop.apps.example.com", "maps", 0),
		host("a.b.apps.example.com", "maps", 0))
	f.remoteCluster("edge2", []string{"192.0.2.9"}, host("shared.apps.example.com", "tiles", 5))
	f.reconcile()

	// Each remote hostname points at its own cluster, the private address
	// left out; the wildcard still points at the console's.
	if v := f.values("example.com", "router.apps", "A"); !slices.Equal(v, []string{"198.51.100.7"}) {
		t.Errorf("router A = %v", v)
	}
	if v := f.values("example.com", "router.apps", "AAAA"); !slices.Equal(v, []string{"2001:db8::7"}) {
		t.Errorf("router AAAA = %v", v)
	}
	if set, _ := f.hz.Get("example.com", "router.apps", "A"); !ownedBy(set, dnsInstance) {
		t.Errorf("router record not labelled as Kwerft's: %+v", set.Labels)
	}
	if v := f.values("example.com", "*.apps", "A"); !slices.Equal(v, []string{"203.0.113.24"}) {
		t.Errorf("wildcard A = %v", v)
	}
	// The older claim wins a hostname two clusters want.
	if v := f.values("example.com", "shared.apps", "A"); !slices.Equal(v, []string{"192.0.2.9"}) {
		t.Errorf("shared A = %v, want edge2's (older claim)", v)
	}
	// The console's own cluster keeps its hostnames; deeper names get none.
	for _, name := range []string{"shop.apps", "a.b.apps"} {
		if _, ok := f.hz.Get("example.com", name, "A"); ok {
			t.Errorf("record created for %s", name)
		}
	}

	if r := f.clusterRecord("edge", "router.apps.example.com"); r.State != kwerftv1.DNSManaged || r.Purpose != purposeApp || r.Project != "maps" ||
		r.Zone != "example.com" || !slices.Equal(r.Values, []string{"198.51.100.7", "2001:db8::7"}) {
		t.Errorf("router status %+v", r)
	}
	if r := f.clusterRecord("edge", "shared.apps.example.com"); r.State != kwerftv1.DNSConflict || !strings.Contains(r.Message, "edge2") {
		t.Errorf("shared status on edge %+v", r)
	}
	if r := f.clusterRecord("edge2", "shared.apps.example.com"); r.State != kwerftv1.DNSManaged || r.Project != "tiles" {
		t.Errorf("shared status on edge2 %+v", r)
	}
	if r := f.clusterRecord("edge", "shop.apps.example.com"); r.State != kwerftv1.DNSConflict || !strings.Contains(r.Message, "project shop") {
		t.Errorf("shop status %+v", r)
	}
	if r := f.clusterRecord("edge", "a.b.apps.example.com"); r.State != kwerftv1.DNSUnsupported || !strings.Contains(r.Message, "a-b.apps.example.com") {
		t.Errorf("a.b status %+v", r)
	}
	// The console's own status lists only its own hostnames.
	if s := f.settings(); len(s.Status.DNS.Records) != 2 {
		t.Errorf("console records %+v", s.Status.DNS.Records)
	}

	writes := f.hz.Writes()
	f.reconcile()
	if f.hz.Writes() != writes {
		t.Errorf("a sync without changes wrote %d times", f.hz.Writes()-writes)
	}

	// edge2 goes away: the hostname moves to edge.
	if err := f.c.Delete(context.Background(), f.cluster("edge2")); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if v := f.values("example.com", "shared.apps", "A"); !slices.Equal(v, []string{"198.51.100.7"}) {
		t.Errorf("shared A = %v after edge2 left", v)
	}

	// The cluster's addresses are unknown for a while: its records stay.
	f.remoteCluster("edge", nil, host("router.apps.example.com", "maps", 0))
	f.reconcile()
	if v := f.values("example.com", "router.apps", "A"); !slices.Equal(v, []string{"198.51.100.7"}) {
		t.Errorf("router A = %v while the addresses are unknown", v)
	}
	if r := f.clusterRecord("edge", "router.apps.example.com"); r.State != kwerftv1.DNSPending || !slices.Equal(r.Values, []string{"198.51.100.7", "2001:db8::7"}) {
		t.Errorf("router status while waiting %+v", r)
	}
	// ...while the hostname it gave up loses its record.
	if _, ok := f.hz.Get("example.com", "shared.apps", "A"); ok {
		t.Error("shared record left behind")
	}

	// The app goes: so does its record; the wildcard stays.
	f.remoteCluster("edge", []string{"198.51.100.7"})
	f.reconcile()
	if names := f.hz.Names("example.com"); len(names) != 2 {
		t.Errorf("records = %v, want the console's and the wildcard only", names)
	}
	if c := f.cluster("edge"); c.Status.DNS != nil {
		t.Errorf("edge DNS status kept: %+v", c.Status.DNS)
	}
}

func TestDNSRemoteClustersLeaveForeignRecordsAlone(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	f.rec.RemoteClusters = true
	f.hz.Put("example.com", hetzner.RRSet{Name: "router.apps", Type: "A", Records: []hetzner.Record{{Value: "192.0.2.1"}}})
	f.remoteCluster("edge", []string{"198.51.100.7"}, host("router.apps.example.com", "maps", 0))
	f.reconcile()
	if v := f.values("example.com", "router.apps", "A"); !slices.Equal(v, []string{"192.0.2.1"}) {
		t.Errorf("foreign record changed: %v", v)
	}
	if r := f.clusterRecord("edge", "router.apps.example.com"); r.State != kwerftv1.DNSConflict {
		t.Errorf("status %+v", r)
	}
}

func TestDNSRemoteClustersWithoutManagedRecords(t *testing.T) {
	spec := managed("apps.example.com")
	spec.DNS.ManageRecords = false
	f := newDNSFixture(t, spec, served("203.0.113.24"), "example.com")
	f.rec.RemoteClusters = true
	f.remoteCluster("edge", []string{"198.51.100.7"}, host("router.apps.example.com", "maps", 0))
	f.remoteCluster("quiet", []string{"198.51.100.8"})
	f.reconcile()
	if c := f.cluster("edge"); c.Status.DNS == nil || !strings.Contains(c.Status.DNS.Message, "by hand") {
		t.Errorf("edge status %+v", c.Status.DNS)
	}
	if c := f.cluster("quiet"); c.Status.DNS != nil {
		t.Errorf("a cluster without hostnames got %+v", c.Status.DNS)
	}
	if f.hz.Writes() != 0 {
		t.Errorf("%d writes with managed records off", f.hz.Writes())
	}
}

func TestDNSInAgentModeIgnoresClusters(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	f.remoteCluster("edge", []string{"198.51.100.7"}, host("router.apps.example.com", "maps", 0))
	f.reconcile()
	if _, ok := f.hz.Get("example.com", "router.apps", "A"); ok {
		t.Error("a reconciler without RemoteClusters wrote a remote cluster's record")
	}
}

func TestHeldHostnames(t *testing.T) {
	d := func(ns, host, listener string, gen, observed int64, at int) kwerftv1.Domain {
		return kwerftv1.Domain{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "d", Generation: gen, CreationTimestamp: since(at)},
			Spec:       kwerftv1.DomainSpec{Hostname: host},
			Status:     kwerftv1.DomainStatus{Listener: listener, ObservedGeneration: observed},
		}
	}
	got := heldHostnames([]kwerftv1.Domain{
		d("maps", "router.apps.example.com", "d-router", 1, 1, 5),
		d("other", "router.apps.example.com", "d-router", 1, 1, 9), // a later claim of the same name
		d("maps", "api.example.org", "d-api", 1, 1, 0),             // not under the apps domain
		d("maps", "new.apps.example.com", "", 1, 0, 0),             // no listener yet
		d("maps", "old.apps.example.com", "d-old", 2, 1, 0),        // stale status
		d("maps", "a.apps.example.com", WildcardListener, 1, 1, 1),
	}, "apps.example.com")
	want := []kwerftv1.ClusterHostname{host("a.apps.example.com", "maps", 1), host("router.apps.example.com", "maps", 5)}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].Hostname != want[i].Hostname || got[i].Project != want[i].Project || !got[i].Since.Equal(&want[i].Since) {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
