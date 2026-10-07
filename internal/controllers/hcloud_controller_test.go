// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

// The Hetzner Cloud reconciler against a fake client and the fake Cloud
// API: a cluster whose first node is the installer's server (not labelled),
// a node pool server Kwerft created (labelled kwerft.dev/cluster=local) and
// a dedicated server, all in one project with a private network.

const hcToken = "hc-test-token"

func init() { hetzner.ActionPoll = time.Millisecond }

type hcFixture struct {
	t     *testing.T
	c     client.Client
	hz    *hetznertest.Server
	rec   *HetznerCloudReconciler
	clock *offsetClock
	net   int64
}

func node(name, platform, ip string, extra ...corev1.NodeAddress) *corev1.Node {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{LabelPlatform: platform}}}
	n.Status.Addresses = append([]corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: ip}}, extra...)
	return n
}

func ruleObj(name string, spec kwerftv1.FirewallRuleSpec, required bool) *kwerftv1.FirewallRule {
	r := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if required {
		r.Labels = map[string]string{firewall.LabelRequired: "true"}
	}
	return r
}

func newHCFixture(t *testing.T, objs ...client.Object) *hcFixture {
	t.Helper()
	hz := hetznertest.New(t, hcToken)
	hz.EnableFirewalls()
	hz.EnableNetworks()
	hz.EnableLoadBalancers()
	netID := hz.PutNetwork(hetzner.Network{Name: "kwerft", IPRange: "10.0.0.0/16"})
	hz.AddServer(hetznertest.NewServerSummary(1, "kwerft-1", "fsn1", "203.0.113.10", "2001:db8:1::/64", netID, "10.0.0.2", nil))
	hz.AddServer(hetznertest.NewServerSummary(2, "local-workers-1", "fsn1", "203.0.113.11", "2001:db8:2::/64", netID, "10.0.0.3",
		map[string]string{CloudLabelCluster: "local", "kwerft.dev/pool": "workers"}))
	hz.AddServer(hetznertest.NewServerSummary(3, "someone-elses", "nbg1", "203.0.113.99", "2001:db8:3::/64", 0, "", nil))

	base := []client.Object{
		&kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(dnsInstance)}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: HCloudTokenSecret, Namespace: GatewayNamespace},
			Data: map[string][]byte{HCloudTokenKey: []byte(hcToken)}},
		node("kwerft-1", "cloud", "203.0.113.10", corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}),
		node("local-workers-1", "cloud", "203.0.113.11"),
		node("dedi-1", "dedicated", "198.51.100.200"),
	}
	for _, r := range firewall.Required("10.0.0.0/16") {
		base = append(base, r.DeepCopy())
	}
	c := fake.NewClientBuilder().WithScheme(NewScheme()).
		WithStatusSubresource(&kwerftv1.ConsoleSettings{}, &kwerftv1.FirewallRule{}).
		WithObjects(append(base, objs...)...).Build()
	clock := &offsetClock{}
	return &hcFixture{t: t, c: c, hz: hz, clock: clock, net: netID,
		rec: &HetznerCloudReconciler{Client: c, HetznerAPI: hz.URL, ProxyNetwork: "10.0.0.0/16", Now: clock.Now}}
}

func (f *hcFixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.rec.Reconcile(context.Background(), hcloudRequest)
	if err != nil {
		f.t.Fatalf("reconcile: %v", err)
	}
	return res
}

func (f *hcFixture) settings() *kwerftv1.ConsoleSettings {
	f.t.Helper()
	var s kwerftv1.ConsoleSettings
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		f.t.Fatal(err)
	}
	return &s
}

func (f *hcFixture) status() *kwerftv1.HetznerCloudStatus {
	f.t.Helper()
	st := f.settings().Status.HetznerCloud
	if st == nil {
		f.t.Fatal("no hetznerCloud status")
	}
	return st
}

func (f *hcFixture) spec(change func(*kwerftv1.HetznerCloudSettings)) {
	f.t.Helper()
	s := f.settings()
	if s.Spec.HetznerCloud == nil {
		s.Spec.HetznerCloud = &kwerftv1.HetznerCloudSettings{}
	}
	change(s.Spec.HetznerCloud)
	if err := f.c.Update(context.Background(), s); err != nil {
		f.t.Fatal(err)
	}
}

// snapshot writes the firewall controller's ConfigMap as if the rules were
// confirmed on the nodes.
func (f *hcFixture) snapshot(rules ...*kwerftv1.FirewallRule) {
	f.t.Helper()
	snap := FirewallSnapshot{Revision: "rev-1"}
	for _, r := range rules {
		snap.Rules = append(snap.Rules, FirewallSnapshotEntry{Name: r.Name, Required: firewall.IsRequired(r), Spec: r.Spec})
	}
	raw, _ := json.Marshal(snap)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap},
		Data: map[string]string{firewall.SnapshotKey: string(raw)}}
	if err := f.c.Delete(context.Background(), cm); err != nil && client.IgnoreNotFound(err) != nil {
		f.t.Fatal(err)
	}
	if err := f.c.Create(context.Background(), cm); err != nil {
		f.t.Fatal(err)
	}
}

func (f *hcFixture) currentRules() []*kwerftv1.FirewallRule {
	var list kwerftv1.FirewallRuleList
	if err := f.c.List(context.Background(), &list); err != nil {
		f.t.Fatal(err)
	}
	out := []*kwerftv1.FirewallRule{}
	for i := range list.Items {
		out = append(out, &list.Items[i])
	}
	return out
}

func (f *hcFixture) ruleState(name string) string {
	f.t.Helper()
	var r kwerftv1.FirewallRule
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: name}, &r); err != nil {
		f.t.Fatal(err)
	}
	return r.Status.CloudFirewall
}

func (f *hcFixture) firewall() hetzner.Firewall {
	f.t.Helper()
	fws := f.hz.Firewalls()
	if len(fws) != 1 {
		f.t.Fatalf("%d firewalls: %+v", len(fws), fws)
	}
	return fws[0]
}

func cloudRule(fw hetzner.Firewall, proto, port string) *hetzner.FirewallRule {
	for i, r := range fw.Rules {
		if r.Protocol == proto && r.Port == port {
			return &fw.Rules[i]
		}
	}
	return nil
}

func appliedKeys(fw hetzner.Firewall) []string {
	var out []string
	for _, a := range fw.AppliedTo {
		out = append(out, a.Key())
	}
	slices.Sort(out)
	return out
}

func TestHCloudWithoutToken(t *testing.T) {
	f := newHCFixture(t)
	if err := f.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: HCloudTokenSecret, Namespace: GatewayNamespace}}); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if st := f.status(); st.Message == "" || st.Firewall != nil {
		t.Errorf("status = %+v", st)
	}
	if got := f.ruleState("ssh"); got != "" {
		t.Errorf("ssh cloudFirewall = %q without a token", got)
	}
	if f.hz.Writes() != 0 {
		t.Error("wrote to the Cloud API without a token")
	}
}

func TestCloudFirewallCreatedFromConfirmedRules(t *testing.T) {
	db := ruleObj("postgres", kwerftv1.FirewallRuleSpec{Port: 5432, Protocol: "TCP", Sources: []string{"198.51.100.7"}, Nodes: "all"}, false)
	f := newHCFixture(t, db)
	f.snapshot(f.currentRules()...)
	f.reconcile()

	fw := f.firewall()
	if fw.Name != "kwerft-local-11111111" || fw.Labels[CloudLabelCluster] != "local" || fw.Labels[DNSLabelInstance] != dnsInstance {
		t.Errorf("firewall %q labels %v", fw.Name, fw.Labels)
	}
	// Selector for Kwerft's servers, the installer's server by ID; never
	// the dedicated node or another project's server.
	if got := appliedKeys(fw); !slices.Equal(got, []string{"label_selector:kwerft.dev/cluster=local", "server:1"}) {
		t.Errorf("applied to %v", got)
	}
	if fw.Servers() != 2 {
		t.Errorf("servers = %d", fw.Servers())
	}
	for _, want := range []struct{ proto, port string }{{"tcp", "22"}, {"tcp", "80"}, {"tcp", "443"}, {"udp", "51871"}, {"icmp", ""}} {
		r := cloudRule(fw, want.proto, want.port)
		if r == nil || !slices.Equal(r.SourceIPs, firewall.Anywhere) {
			t.Errorf("%s %s: %+v", want.proto, want.port, r)
		}
	}
	if r := cloudRule(fw, "tcp", "5432"); r == nil || !slices.Equal(r.SourceIPs, []string{"198.51.100.7/32"}) {
		t.Errorf("postgres: %+v", r)
	}
	if len(fw.Rules) != 6 {
		t.Errorf("rules: %+v", fw.Rules)
	}

	st := f.status()
	if st.Firewall == nil || st.Firewall.State != "InSync" || st.Firewall.Rules != 6 || st.Firewall.Servers != 2 || st.Firewall.Revision != "rev-1" {
		t.Errorf("firewall status %+v", st.Firewall)
	}
	if len(st.Servers) != 2 || st.Servers[0].Node != "kwerft-1" || st.Servers[0].Labelled || !st.Servers[1].Labelled {
		t.Errorf("servers %+v", st.Servers)
	}
	for name, want := range map[string]string{"ssh": "Applied", "postgres": "Applied", "cluster-private": "Private", "icmp": "Applied"} {
		if got := f.ruleState(name); got != want {
			t.Errorf("%s: cloudFirewall %q, want %q", name, got, want)
		}
	}
	// Nothing changes on the next pass.
	writes := f.hz.Writes()
	f.reconcile()
	if f.hz.Writes() != writes {
		t.Errorf("an unchanged pass wrote %d times", f.hz.Writes()-writes)
	}
}

func TestCloudFirewallWaitsForConfirmation(t *testing.T) {
	f := newHCFixture(t)
	f.snapshot(f.currentRules()...)
	f.reconcile()

	// SSH narrowed in the console but not yet confirmed on the hosts.
	var ssh kwerftv1.FirewallRule
	if err := f.c.Get(context.Background(), client.ObjectKey{Name: "ssh"}, &ssh); err != nil {
		t.Fatal(err)
	}
	ssh.Spec.Sources = []string{"192.0.2.0/24"}
	if err := f.c.Update(context.Background(), &ssh); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if r := cloudRule(f.firewall(), "tcp", "22"); !slices.Equal(r.SourceIPs, firewall.Anywhere) {
		t.Errorf("an unconfirmed narrowing reached the Cloud Firewall: %+v", r)
	}
	if got := f.ruleState("ssh"); got != firewall.CloudPending {
		t.Errorf("ssh: %q", got)
	}
	if st := f.status().Firewall; st.State != "Applying" {
		t.Errorf("state %+v", st)
	}

	// Confirmed: now it follows.
	f.snapshot(f.currentRules()...)
	f.reconcile()
	if r := cloudRule(f.firewall(), "tcp", "22"); !slices.Equal(r.SourceIPs, []string{"192.0.2.0/24"}) {
		t.Errorf("confirmed narrowing: %+v", r)
	}
	if got := f.ruleState("ssh"); got != firewall.CloudApplied {
		t.Errorf("ssh: %q", got)
	}
	// HTTP(S) and WireGuard stay open to everyone whatever happens.
	if !firewall.CloudKeepsBaseline(f.firewall().Rules) {
		t.Error("baseline lost")
	}
}

func TestCloudFirewallWithoutSnapshotKeepsSSHOpen(t *testing.T) {
	f := newHCFixture(t)
	var ssh kwerftv1.FirewallRule
	_ = f.c.Get(context.Background(), client.ObjectKey{Name: "ssh"}, &ssh)
	ssh.Spec.Sources = []string{"192.0.2.0/24"}
	if err := f.c.Update(context.Background(), &ssh); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	if r := cloudRule(f.firewall(), "tcp", "22"); !slices.Equal(r.SourceIPs, firewall.Anywhere) {
		t.Errorf("ssh: %+v", r)
	}
}

func TestCloudFirewallRepairsDriftAndFollowsNodes(t *testing.T) {
	f := newHCFixture(t)
	f.snapshot(f.currentRules()...)
	f.reconcile()
	fw := f.firewall()

	// Someone deletes the SSH rule in the Hetzner Console.
	if err := f.hz.Client().SetFirewallRules(context.Background(), fw.ID, fw.Rules[1:]); err != nil {
		t.Fatal(err)
	}
	// The installer's server leaves the cluster.
	if err := f.c.Delete(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "kwerft-1"}}); err != nil {
		t.Fatal(err)
	}
	f.reconcile()
	fw = f.firewall()
	if cloudRule(fw, "tcp", "22") == nil {
		t.Error("the SSH rule was not put back")
	}
	if got := appliedKeys(fw); !slices.Equal(got, []string{"label_selector:kwerft.dev/cluster=local"}) {
		t.Errorf("applied to %v", got)
	}
}

func TestCloudFirewallOff(t *testing.T) {
	f := newHCFixture(t)
	f.reconcile()
	f.firewall()
	f.spec(func(h *kwerftv1.HetznerCloudSettings) { h.Firewall = kwerftv1.CloudFirewallOff })
	f.reconcile()
	if n := len(f.hz.Firewalls()); n != 0 {
		t.Errorf("%d firewalls left", n)
	}
	if st := f.status().Firewall; st.State != "Off" {
		t.Errorf("state %+v", st)
	}
	if got := f.ruleState("http"); got != firewall.CloudOff {
		t.Errorf("http: %q", got)
	}
}

func TestCloudFirewallLeavesForeignFirewallsAlone(t *testing.T) {
	f := newHCFixture(t)
	other := f.hz.PutFirewall(hetzner.Firewall{Name: "kwerft-local-11111111", Labels: map[string]string{CloudLabelCluster: "local"}})
	f.reconcile()
	if st := f.status().Firewall; st.State != "Error" || st.Message == "" {
		t.Errorf("state %+v", st)
	}
	fws := f.hz.Firewalls()
	if len(fws) != 1 || fws[0].ID != other || len(fws[0].Rules) != 0 {
		t.Errorf("foreign firewall changed: %+v", fws)
	}
}

func TestHCloudReadOnlyToken(t *testing.T) {
	f := newHCFixture(t)
	f.hz.SetReadOnly(true)
	res := f.reconcile()
	if st := f.status(); st.Message == "" || res.RequeueAfter == 0 {
		t.Errorf("status %+v, requeue %v", st, res.RequeueAfter)
	}
}

func TestLoadBalancer(t *testing.T) {
	f := newHCFixture(t)
	f.hz.SetTargetHealth(1, "unhealthy")
	f.hz.SetTargetHealth(2, "unhealthy")
	f.spec(func(h *kwerftv1.HetznerCloudSettings) { h.LoadBalancer = &kwerftv1.LoadBalancerSettings{Enabled: true} })
	f.reconcile()

	lbs := f.hz.LoadBalancers()
	if len(lbs) != 1 {
		t.Fatalf("load balancers: %+v", lbs)
	}
	lb := lbs[0]
	if lb.LoadBalancerType.Name != "lb11" || lb.Location.Name != "fsn1" || !lb.AttachedTo(f.net) {
		t.Errorf("created %+v", lb)
	}
	if len(lb.Services) != 2 || !lb.Services[0].Proxyprotocol || !lb.Services[1].Proxyprotocol {
		t.Errorf("services %+v", lb.Services)
	}
	keys := []string{}
	for _, tg := range lb.Targets {
		keys = append(keys, tg.Key())
		if !tg.UsePrivateIP {
			t.Errorf("target %s over the public network", tg.Key())
		}
	}
	slices.Sort(keys)
	if !slices.Equal(keys, []string{"label_selector:kwerft.dev/cluster=local", "server:1"}) {
		t.Errorf("targets %v", keys)
	}
	st := f.status().LoadBalancer
	if st.State != "Waiting" || st.Active || st.Targets != 2 || st.HealthyTargets != 0 {
		t.Errorf("unhealthy: %+v", st)
	}
	if LoadBalancerAddresses(f.settings()) != nil {
		t.Error("DNS moved before a target was healthy")
	}

	f.hz.SetTargetHealth(1, "healthy")
	f.reconcile()
	st = f.status().LoadBalancer
	if st.State != "Active" || !st.Active || st.HealthyTargets != 1 {
		t.Errorf("healthy: %+v", st)
	}
	if got := LoadBalancerAddresses(f.settings()); !slices.Equal(got, []string{lb.PublicNet.IPv4.IP, lb.PublicNet.IPv6.IP}) {
		t.Errorf("addresses %v", got)
	}

	// A service changed by hand is put back.
	_ = f.hz.Client().UpdateLoadBalancerService(context.Background(), lb.ID, hetzner.LBService{Protocol: "tcp", ListenPort: 443, DestinationPort: 8443,
		HealthCheck: hetzner.LBHealthCheck{Protocol: "tcp", Port: 8443, Interval: 15, Timeout: 10, Retries: 3}})
	f.reconcile()
	if svc := f.hz.LoadBalancers()[0].Services[1]; svc.DestinationPort != 443 || !svc.Proxyprotocol {
		t.Errorf("service not repaired: %+v", svc)
	}

	// Off: DNS moves back at once, the Load Balancer goes after the drain.
	f.spec(func(h *kwerftv1.HetznerCloudSettings) { h.LoadBalancer.Enabled = false })
	f.reconcile()
	st = f.status().LoadBalancer
	if st.State != "Draining" || st.DrainingSince == nil || LoadBalancerAddresses(f.settings()) != nil {
		t.Errorf("draining: %+v", st)
	}
	if len(f.hz.LoadBalancers()) != 1 {
		t.Error("deleted before the drain")
	}
	f.clock.Advance(LBDrain + time.Second)
	f.reconcile()
	if len(f.hz.LoadBalancers()) != 0 || f.status().LoadBalancer != nil {
		t.Errorf("not deleted after the drain: %+v", f.status().LoadBalancer)
	}
}

func TestLoadBalancerNeedsProxyNetwork(t *testing.T) {
	f := newHCFixture(t)
	f.rec.ProxyNetwork = ""
	f.spec(func(h *kwerftv1.HetznerCloudSettings) { h.LoadBalancer = &kwerftv1.LoadBalancerSettings{Enabled: true} })
	f.reconcile()
	if st := f.status().LoadBalancer; st.State != "Error" || st.Message == "" {
		t.Errorf("status %+v", st)
	}
	if len(f.hz.LoadBalancers()) != 0 {
		t.Error("created without the PROXY protocol on the ingress")
	}
}

func TestHCloudTokenMirroredToCSI(t *testing.T) {
	csi := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret,
		Labels: map[string]string{LabelManagedBy: ManagedByKwerft}}, Data: map[string][]byte{HCloudTokenKey: []byte("old")}}
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: "hcloud-csi-controller"}}
	f := newHCFixture(t, csi, deploy)
	f.reconcile()
	var got corev1.Secret
	_ = f.c.Get(context.Background(), client.ObjectKeyFromObject(csi), &got)
	if string(got.Data[HCloudTokenKey]) != hcToken {
		t.Errorf("token not mirrored: %q", got.Data[HCloudTokenKey])
	}
	var d appsv1.Deployment
	_ = f.c.Get(context.Background(), client.ObjectKeyFromObject(deploy), &d)
	if d.Spec.Template.Annotations[annotationHCloudTokenSum] == "" {
		t.Error("CSI controller not restarted")
	}

	// The operator's own Secret is left alone.
	f2 := newHCFixture(t, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret},
		Data: map[string][]byte{HCloudTokenKey: []byte("theirs")}})
	f2.reconcile()
	_ = f2.c.Get(context.Background(), client.ObjectKeyFromObject(csi), &got)
	if string(got.Data[HCloudTokenKey]) != "theirs" {
		t.Errorf("foreign Secret changed: %q", got.Data[HCloudTokenKey])
	}
}

func TestMatchCloudNodesByProviderID(t *testing.T) {
	n := node("renamed", "cloud", "192.0.2.1")
	n.Spec.ProviderID = "hcloud://2"
	srv := hetznertest.NewServerSummary(2, "local-workers-1", "fsn1", "203.0.113.11", "2001:db8:2::/64", 0, "", map[string]string{CloudLabelCluster: "local"})
	other := hetznertest.NewServerSummary(5, "x", "fsn1", "192.0.2.1", "2001:db8:5::/64", 0, "", nil)
	got := MatchCloudNodes([]corev1.Node{*n}, []hetzner.ServerSummary{other, srv}, "local")
	if len(got) != 1 || got[0].Server.ID != 2 || !got[0].Labelled {
		t.Errorf("got %+v", got)
	}
}

// TestMatchCloudNodesIgnoresPrivateAddresses: a server of the token's
// project with the same private address in another Cloud Network is not
// this cluster's node.
func TestMatchCloudNodesIgnoresPrivateAddresses(t *testing.T) {
	n := node("kwerft-dev-test", "cloud", "198.51.100.20")
	n.Status.Addresses = append(n.Status.Addresses, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.2"})
	elsewhere := hetznertest.NewServerSummary(9, "production-1", "fsn1", "203.0.113.50", "2001:db8:9::/64", 77, "10.0.0.2", nil)
	if got := MatchCloudNodes([]corev1.Node{*n}, []hetzner.ServerSummary{elsewhere}, "local"); len(got) != 0 {
		t.Fatalf("matched by private address: %+v", got)
	}
	self := hetznertest.NewServerSummary(10, "kwerft-dev-test", "nbg1", "198.51.100.20", "2001:db8:10::/64", 78, "10.0.0.2", nil)
	if got := MatchCloudNodes([]corev1.Node{*n}, []hetzner.ServerSummary{elsewhere, self}, "local"); len(got) != 1 || got[0].Server.ID != 10 {
		t.Fatalf("got %+v", got)
	}
}

func TestRemoteClusterUsesTheCSISecret(t *testing.T) {
	// A remote Cloud cluster has only kube-system/hcloud (from the console).
	csi := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret},
		Data: map[string][]byte{HCloudTokenKey: []byte(hcToken)}}
	f := newHCFixture(t, csi)
	if err := f.c.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: HCloudTokenSecret, Namespace: GatewayNamespace}}); err != nil {
		t.Fatal(err)
	}
	// The management cluster does not fall back: a removed token stops the sync.
	f.reconcile()
	if len(f.hz.Firewalls()) != 0 {
		t.Fatal("the management cluster used the CSI driver's token")
	}
	f.rec.ClusterName = "second"
	f.reconcile()
	fws := f.hz.Firewalls()
	if len(fws) != 1 || fws[0].Labels[CloudLabelCluster] != "second" || !slices.Contains(appliedKeys(fws[0]), "label_selector:kwerft.dev/cluster=second") {
		t.Errorf("remote cluster's firewall: %+v", fws)
	}
}
