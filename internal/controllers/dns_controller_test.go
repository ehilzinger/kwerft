package controllers

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

// The DNS reconciler against a fake client and a fake Hetzner API: it needs
// no API server, and the Domain reconciler's part of the status (public
// addresses, the active console hostname) is set by hand.

const (
	dnsToken    = "hz-test-token"
	dnsInstance = "11111111-2222-3333-4444-555555555555"
)

type dnsFixture struct {
	t   *testing.T
	c   client.Client
	hz  *hetznertest.Server
	rec *DNSReconciler
}

func newDNSFixture(t *testing.T, spec kwerftv1.ConsoleSettingsSpec, status kwerftv1.ConsoleSettingsStatus, zones ...string) *dnsFixture {
	t.Helper()
	hz := hetznertest.New(t, dnsToken, zones...)
	settings := &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}, Spec: spec, Status: status}
	c := fake.NewClientBuilder().WithScheme(NewScheme()).
		WithStatusSubresource(&kwerftv1.ConsoleSettings{}, &kwerftv1.Cluster{}, &kwerftv1.Domain{}).
		WithObjects(settings,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(dnsInstance)}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: DNSTokenSecret, Namespace: GatewayNamespace},
				Data: map[string][]byte{DNSTokenKey: []byte(dnsToken)}}).
		Build()
	return &dnsFixture{t: t, c: c, hz: hz, rec: &DNSReconciler{Client: c, HetznerAPI: hz.URL}}
}

func managed(apps string) kwerftv1.ConsoleSettingsSpec {
	return kwerftv1.ConsoleSettingsSpec{ConsoleDomain: "ops.example.com", AppsDomain: apps,
		TLS: kwerftv1.TLSHTTP01, DNS: &kwerftv1.DNSSettings{Provider: "hetzner", ManageRecords: true}}
}

func served(addrs ...string) kwerftv1.ConsoleSettingsStatus {
	return kwerftv1.ConsoleSettingsStatus{ConsoleDomain: "ops.example.com", PublicAddresses: addrs}
}

func (f *dnsFixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.rec.Reconcile(context.Background(), dnsRequest)
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *dnsFixture) settings() *kwerftv1.ConsoleSettings {
	f.t.Helper()
	var s kwerftv1.ConsoleSettings
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

func (f *dnsFixture) update(change func(*kwerftv1.ConsoleSettings)) {
	f.t.Helper()
	s := f.settings()
	change(s)
	// Update ignores the status and returns the stored one.
	status := *s.Status.DeepCopy()
	if err := f.c.Update(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
	s.Status = status
	if err := f.c.Status().Update(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
}

func (f *dnsFixture) record(host string) kwerftv1.DNSRecordStatus {
	f.t.Helper()
	s := f.settings()
	if s.Status.DNS == nil {
		f.t.Fatalf("no DNS status")
	}
	for _, r := range s.Status.DNS.Records {
		if r.Hostname == host {
			return r
		}
	}
	f.t.Fatalf("no DNS status for %s in %+v", host, s.Status.DNS.Records)
	return kwerftv1.DNSRecordStatus{}
}

func (f *dnsFixture) values(zone, name, typ string) []string {
	f.t.Helper()
	set, ok := f.hz.Get(zone, name, typ)
	if !ok {
		return nil
	}
	v := set.Values()
	slices.Sort(v)
	return v
}

func ownedBy(set hetzner.RRSet, instance string) bool {
	return set.Labels[DNSLabelManagedBy] == ManagedByKwerft && set.Labels[DNSLabelInstance] == instance
}

func TestDNSCreatesConsoleAndWildcardRecords(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24", "10.0.0.2"), "example.com")
	res := f.reconcile()
	if res.RequeueAfter != dnsResync {
		t.Errorf("requeue after %v, want the resync interval", res.RequeueAfter)
	}

	for _, name := range []string{"ops", "*.apps"} {
		set, ok := f.hz.Get("example.com", name, "A")
		if !ok {
			t.Fatalf("no A record for %s; zone has %v", name, f.hz.Names("example.com"))
		}
		// The private node address is never published.
		if !slices.Equal(set.Values(), []string{"203.0.113.24"}) || !ownedBy(set, dnsInstance) || set.TTL == nil || *set.TTL != dnsTTL {
			t.Errorf("%s: %+v", name, set)
		}
	}
	if names := f.hz.Names("example.com"); len(names) != 2 {
		t.Errorf("records = %v, want ops/A and *.apps/A only (no AAAA without an IPv6 address)", names)
	}
	for _, host := range []string{"ops.example.com", "*.apps.example.com"} {
		if r := f.record(host); r.State != kwerftv1.DNSManaged || r.Zone != "example.com" || !slices.Equal(r.Values, []string{"203.0.113.24"}) {
			t.Errorf("%s: %+v", host, r)
		}
	}
	if s := f.settings(); s.Status.DNS.SyncedAt == nil || s.Status.DNS.Message != "" || !slices.Equal(s.Status.DNS.Zones, []string{"example.com"}) {
		t.Errorf("dns status: %+v", s.Status.DNS)
	}

	// In step: a second sync writes nothing.
	writes := f.hz.Writes()
	f.reconcile()
	if f.hz.Writes() != writes {
		t.Errorf("a sync without changes wrote %d times", f.hz.Writes()-writes)
	}
}

func TestDNSFollowsAddressChanges(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	f.reconcile()

	f.update(func(s *kwerftv1.ConsoleSettings) {
		s.Status.PublicAddresses = []string{"198.51.100.7", "2001:db8::1"}
	})
	f.reconcile()
	if v := f.values("example.com", "ops", "A"); !slices.Equal(v, []string{"198.51.100.7"}) {
		t.Errorf("A = %v", v)
	}
	if v := f.values("example.com", "*.apps", "AAAA"); !slices.Equal(v, []string{"2001:db8::1"}) {
		t.Errorf("AAAA = %v", v)
	}

	// The IPv6 address goes away: so does Kwerft's AAAA record.
	f.update(func(s *kwerftv1.ConsoleSettings) { s.Status.PublicAddresses = []string{"198.51.100.7"} })
	f.reconcile()
	if _, ok := f.hz.Get("example.com", "*.apps", "AAAA"); ok {
		t.Error("AAAA record left behind")
	}
}

func TestDNSLeavesForeignRecordsAlone(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	// Created by hand: the console's points here, the wildcard elsewhere.
	f.hz.Put("example.com", hetzner.RRSet{Name: "ops", Type: "A", Records: []hetzner.Record{{Value: "203.0.113.24"}}})
	f.hz.Put("example.com", hetzner.RRSet{Name: "*.apps", Type: "A", Records: []hetzner.Record{{Value: "192.0.2.1"}}})
	f.reconcile()

	if r := f.record("ops.example.com"); r.State != kwerftv1.DNSExternal {
		t.Errorf("console: %+v", r)
	}
	r := f.record("*.apps.example.com")
	if r.State != kwerftv1.DNSConflict || !strings.Contains(r.Message, "192.0.2.1") || !slices.Equal(r.Values, []string{"192.0.2.1"}) {
		t.Errorf("wildcard: %+v", r)
	}
	if f.hz.Writes() != 0 {
		t.Errorf("wrote %d times to records Kwerft did not create", f.hz.Writes())
	}
	if set, _ := f.hz.Get("example.com", "*.apps", "A"); set.Labels != nil {
		t.Errorf("labelled a foreign record: %+v", set)
	}
}

func TestDNSDoesNotReplaceCNAME(t *testing.T) {
	f := newDNSFixture(t, managed(""), served("203.0.113.24"), "example.com")
	f.hz.Put("example.com", hetzner.RRSet{Name: "ops", Type: "CNAME", Records: []hetzner.Record{{Value: "lb.example.net."}}})
	f.reconcile()
	if r := f.record("ops.example.com"); r.State != kwerftv1.DNSConflict || !strings.Contains(r.Message, "CNAME") {
		t.Errorf("console: %+v", r)
	}
	if f.hz.Writes() != 0 {
		t.Errorf("wrote %d times", f.hz.Writes())
	}
}

func TestDNSMovesWithTheSettings(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com", "example.org")
	f.reconcile()

	// A console move in progress: the new name gets its record at once (its
	// HTTP-01 certificate needs it), the old one keeps its own.
	f.update(func(s *kwerftv1.ConsoleSettings) {
		s.Spec.ConsoleDomain = "console.example.org"
		s.Spec.AppsDomain = "apps.example.org"
	})
	f.reconcile()
	if r := f.record("console.example.org"); r.Purpose != "console-next" || r.State != kwerftv1.DNSManaged {
		t.Errorf("new console: %+v", r)
	}
	if v := f.values("example.org", "console", "A"); v == nil {
		t.Error("no record for the new console hostname")
	}
	if v := f.values("example.org", "*.apps", "A"); v == nil {
		t.Error("no record for the new apps domain")
	}
	// The old apps wildcard is Kwerft's and no longer wanted.
	if _, ok := f.hz.Get("example.com", "*.apps", "A"); ok {
		t.Error("old wildcard record left behind")
	}
	if _, ok := f.hz.Get("example.com", "ops", "A"); !ok {
		t.Error("the console's current record was removed before the move")
	}

	// The move completes; the old name redirects for a day, then goes.
	f.update(func(s *kwerftv1.ConsoleSettings) {
		s.Status.ConsoleDomain, s.Status.PreviousConsoleDomain = "console.example.org", "ops.example.com"
	})
	f.reconcile()
	if r := f.record("ops.example.com"); r.Purpose != "console-previous" || r.State != kwerftv1.DNSManaged {
		t.Errorf("previous console: %+v", r)
	}
	f.update(func(s *kwerftv1.ConsoleSettings) { s.Status.PreviousConsoleDomain = "" })
	f.reconcile()
	if _, ok := f.hz.Get("example.com", "ops", "A"); ok {
		t.Error("previous console record left behind after the grace period")
	}
	if names := f.hz.Names("example.com"); len(names) != 0 {
		t.Errorf("example.com still has %v", names)
	}
}

func TestDNSTakesOverFromAnEarlierInstallation(t *testing.T) {
	f := newDNSFixture(t, managed(""), served("203.0.113.24"), "example.com")
	old := map[string]string{DNSLabelManagedBy: ManagedByKwerft, DNSLabelInstance: "old-cluster"}
	f.hz.Put("example.com", hetzner.RRSet{Name: "ops", Type: "A", Labels: old, Records: []hetzner.Record{{Value: "192.0.2.50"}}})
	f.reconcile()

	set, _ := f.hz.Get("example.com", "ops", "A")
	if !ownedBy(set, dnsInstance) || !slices.Equal(set.Values(), []string{"203.0.113.24"}) {
		t.Errorf("not taken over: %+v", set)
	}
	if r := f.record("ops.example.com"); r.State != kwerftv1.DNSManaged || !strings.Contains(r.Message, "Taken over") {
		t.Errorf("status: %+v", r)
	}

	// Now another installation takes it from this one: this one yields.
	f.hz.Put("example.com", hetzner.RRSet{Name: "ops", Type: "A", Labels: map[string]string{DNSLabelManagedBy: ManagedByKwerft, DNSLabelInstance: "new-cluster"},
		Records: []hetzner.Record{{Value: "198.51.100.9"}}})
	writes := f.hz.Writes()
	f.reconcile()
	if r := f.record("ops.example.com"); r.State != kwerftv1.DNSTakenOver || !slices.Equal(r.Values, []string{"198.51.100.9"}) {
		t.Errorf("status: %+v", r)
	}
	if f.hz.Writes() != writes {
		t.Error("fought over a record another installation took")
	}
	f.reconcile()
	if r := f.record("ops.example.com"); r.State != kwerftv1.DNSTakenOver {
		t.Errorf("does not stay yielded: %+v", r)
	}
}

func TestDNSProblems(t *testing.T) {
	t.Run("no zone", func(t *testing.T) {
		f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.net")
		f.reconcile()
		if r := f.record("ops.example.com"); r.State != kwerftv1.DNSNoZone || r.Zone != "" {
			t.Errorf("%+v", r)
		}
	})
	t.Run("secondary zone", func(t *testing.T) {
		f := newDNSFixture(t, managed(""), served("203.0.113.24"))
		f.hz.AddZone("example.com", "secondary")
		f.reconcile()
		if r := f.record("ops.example.com"); r.State != kwerftv1.DNSNoZone || !strings.Contains(r.Message, "secondary") {
			t.Errorf("%+v", r)
		}
	})
	t.Run("token rejected keeps the last records", func(t *testing.T) {
		f := newDNSFixture(t, managed(""), served("203.0.113.24"), "example.com")
		f.reconcile()
		f.hz.SetToken("rotated")
		f.reconcile()
		s := f.settings()
		if !strings.Contains(s.Status.DNS.Message, "rejected") || len(s.Status.DNS.Records) != 1 {
			t.Errorf("%+v", s.Status.DNS)
		}
	})
	t.Run("API down", func(t *testing.T) {
		f := newDNSFixture(t, managed(""), served("203.0.113.24"), "example.com")
		f.hz.FailWith(http.StatusServiceUnavailable)
		if _, err := f.rec.Reconcile(context.Background(), dnsRequest); err == nil {
			t.Error("no error to retry with")
		}
		if s := f.settings(); !strings.Contains(s.Status.DNS.Message, "Could not list") {
			t.Errorf("%+v", s.Status.DNS)
		}
	})
	t.Run("rate limited", func(t *testing.T) {
		f := newDNSFixture(t, managed(""), served("203.0.113.24"), "example.com")
		f.hz.FailWith(http.StatusTooManyRequests)
		if res := f.reconcile(); res.RequeueAfter <= 0 {
			t.Errorf("requeue %v", res.RequeueAfter)
		}
	})
	t.Run("only private addresses", func(t *testing.T) {
		f := newDNSFixture(t, managed(""), served("10.0.0.2"), "example.com")
		f.reconcile()
		if s := f.settings(); !strings.Contains(s.Status.DNS.Message, "public addresses") || f.hz.Writes() != 0 {
			t.Errorf("%+v", s.Status.DNS)
		}
	})
}

func TestDNSOffLeavesRecords(t *testing.T) {
	f := newDNSFixture(t, managed("apps.example.com"), served("203.0.113.24"), "example.com")
	f.reconcile()
	f.update(func(s *kwerftv1.ConsoleSettings) { s.Spec.DNS.ManageRecords = false })
	f.reconcile()
	if s := f.settings(); s.Status.DNS != nil {
		t.Errorf("status kept: %+v", s.Status.DNS)
	}
	if names := f.hz.Names("example.com"); len(names) != 2 {
		t.Errorf("records = %v; turning management off must leave them", names)
	}
}

func TestDNSInputsIgnoreOwnStatus(t *testing.T) {
	s := &kwerftv1.ConsoleSettings{Spec: managed("apps.example.com"), Status: served("203.0.113.24")}
	before := dnsInputs(s)
	s.Status.DNS = &kwerftv1.DNSStatus{Message: "x"}
	s.Status.Certificates = []kwerftv1.CertificateState{{Name: "c"}}
	if dnsInputs(s) != before {
		t.Error("status.dns or certificates changed the inputs")
	}
	s.Status.PublicAddresses = []string{"198.51.100.7"}
	if dnsInputs(s) == before {
		t.Error("an address change did not change the inputs")
	}
}
