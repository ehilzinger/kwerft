package hetzner_test

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

func init() { hetzner.ActionPoll = time.Millisecond }

func cloudFake(t *testing.T) *hetznertest.Server {
	t.Helper()
	srv := hetznertest.New(t, "tok")
	srv.EnableFirewalls()
	srv.EnableNetworks()
	srv.EnableLoadBalancers()
	net := srv.PutNetwork(hetzner.Network{Name: "kwerft", IPRange: "10.0.0.0/16",
		Subnets: []hetzner.Subnet{{Type: "cloud", IPRange: "10.0.0.0/24", NetworkZone: "eu-central"}}})
	srv.AddServer(hetznertest.NewServerSummary(1, "kwerft-1", "fsn1", "203.0.113.10", "2001:db8:1::/64", net, "10.0.0.2", nil))
	srv.AddServer(hetznertest.NewServerSummary(2, "pool-a-1", "fsn1", "203.0.113.11", "2001:db8:2::/64", net, "10.0.0.3",
		map[string]string{"kwerft.dev/cluster": "local", "kwerft.dev/pool": "a"}))
	return srv
}

func TestFirewallLifecycle(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	ctx := context.Background()
	rules := []hetzner.FirewallRule{
		{Direction: "in", Protocol: "tcp", Port: "22", SourceIPs: []string{"0.0.0.0/0", "::/0"}},
		{Direction: "in", Protocol: "icmp", SourceIPs: []string{"0.0.0.0/0", "::/0"}},
	}
	fw, err := c.CreateFirewall(ctx, "kwerft-local", map[string]string{"kwerft.dev/cluster": "local"}, rules,
		[]hetzner.FirewallResource{hetzner.SelectorResource("kwerft.dev/cluster=local"), hetzner.ServerResource(1)})
	if err != nil {
		t.Fatal(err)
	}
	if fw.ID == 0 || len(fw.Rules) != 2 || fw.Servers() != 2 {
		t.Fatalf("created %+v (servers %d)", fw, fw.Servers())
	}
	list, err := c.Firewalls(ctx, "kwerft.dev/cluster=local")
	if err != nil || len(list) != 1 || list[0].ID != fw.ID {
		t.Fatalf("Firewalls = %+v, %v", list, err)
	}
	if list, _ := c.Firewalls(ctx, "kwerft.dev/cluster=other"); len(list) != 0 {
		t.Errorf("selector matched %+v", list)
	}

	if err := c.SetFirewallRules(ctx, fw.ID, rules[:1]); err != nil {
		t.Fatal(err)
	}
	// Applying twice is refused, like the API does.
	var apiErr *hetzner.APIError
	if err := c.ApplyFirewall(ctx, fw.ID, hetzner.ServerResource(1)); !errors.As(err, &apiErr) || apiErr.Code != "firewall_already_applied" {
		t.Errorf("apply twice: %v", err)
	}
	if err := c.ApplyFirewall(ctx, fw.ID, hetzner.ServerResource(99)); !errors.Is(err, hetzner.ErrNotFound) {
		t.Errorf("apply to a missing server: %v", err)
	}
	// In use: no delete.
	if err := c.DeleteFirewall(ctx, fw.ID); !errors.As(err, &apiErr) || apiErr.Code != "resource_in_use" {
		t.Errorf("delete in use: %v", err)
	}
	if err := c.RemoveFirewall(ctx, fw.ID, hetzner.ServerResource(1), hetzner.SelectorResource("kwerft.dev/cluster=local")); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetFirewall(ctx, fw.ID)
	if err != nil || len(got.Rules) != 1 || len(got.AppliedTo) != 0 {
		t.Fatalf("after changes: %+v, %v", got, err)
	}
	if err := c.DeleteFirewall(ctx, fw.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteFirewall(ctx, fw.ID); err != nil {
		t.Errorf("deleting a missing firewall: %v", err)
	}
	// Invalid rules are refused.
	if _, err := c.CreateFirewall(ctx, "bad", nil, []hetzner.FirewallRule{{Direction: "in", Protocol: "tcp", Port: "0", SourceIPs: []string{"0.0.0.0/0"}}}, nil); err == nil {
		t.Error("an invalid port was accepted")
	}
}

func TestWaitActions(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	ctx := context.Background()
	fw, err := c.CreateFirewall(ctx, "fw", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.RunActionsFor(3)
	if err := c.SetFirewallRules(ctx, fw.ID, nil); err != nil {
		t.Fatalf("running action: %v", err)
	}
	srv.RunActionsFor(0)
	srv.FailNextAction("firewall_resource_not_found")
	var ae *hetzner.ActionError
	if err := c.SetFirewallRules(ctx, fw.ID, nil); !errors.As(err, &ae) || ae.Code != "firewall_resource_not_found" {
		t.Errorf("failed action: %v", err)
	}
}

func TestProbeWrite(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	if err := c.ProbeWrite(context.Background()); err != nil {
		t.Errorf("read & write token: %v", err)
	}
	if n := len(srv.Firewalls()); n != 0 {
		t.Errorf("the probe left %d firewalls", n)
	}
	srv.SetReadOnly(true)
	if err := c.ProbeWrite(context.Background()); !errors.Is(err, hetzner.ErrReadOnly) {
		t.Errorf("read-only token: %v", err)
	}
	// A read-only token's refusal still counts as a rejected token elsewhere.
	_, err := c.CreateFirewall(context.Background(), "x", nil, nil, nil)
	if !errors.Is(err, hetzner.ErrTokenRejected) || !errors.Is(err, hetzner.ErrForbidden) {
		t.Errorf("forbidden write: %v", err)
	}
	c.Token = "wrong"
	if err := c.ProbeWrite(context.Background()); !errors.Is(err, hetzner.ErrTokenRejected) || errors.Is(err, hetzner.ErrForbidden) {
		t.Errorf("wrong token: %v", err)
	}
}

func TestServerSummaries(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	all, err := c.ServerSummaries(context.Background(), "")
	if err != nil || len(all) != 2 {
		t.Fatalf("all = %+v, %v", all, err)
	}
	pool, err := c.ServerSummaries(context.Background(), "kwerft.dev/cluster=local")
	if err != nil || len(pool) != 1 || pool[0].Name != "pool-a-1" || pool[0].Location.Name != "fsn1" {
		t.Fatalf("selected = %+v, %v", pool, err)
	}
	s := all[0]
	for addr, want := range map[string]bool{"203.0.113.10": true, "10.0.0.2": false, "2001:db8:1::1": true, "203.0.113.11": false, "::ffff:203.0.113.10": true} {
		if got := s.HasAddress(netip.MustParseAddr(addr)); got != want {
			t.Errorf("HasAddress(%s) = %v", addr, got)
		}
	}
}

func TestNetworks(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	ctx := context.Background()
	n, err := c.CreateNetwork(ctx, hetzner.NetworkOpts{Name: "kwerft-second", IPRange: "10.1.0.0/16", Labels: map[string]string{"kwerft.dev/cluster": "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddSubnet(ctx, n.ID, hetzner.Subnet{Type: "cloud", IPRange: "10.1.0.0/24", NetworkZone: "eu-central"}); err != nil {
		t.Fatal(err)
	}
	got, err := c.Networks(ctx, "kwerft.dev/cluster=second")
	if err != nil || len(got) != 1 || len(got[0].Subnets) != 1 {
		t.Fatalf("Networks = %+v, %v", got, err)
	}
	first, err := c.Networks(ctx, "")
	if err != nil || len(first) != 2 || len(first[0].Servers) != 2 {
		t.Fatalf("servers in the first network: %+v, %v", first, err)
	}
	if err := c.DeleteNetwork(ctx, n.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetNetwork(ctx, n.ID); !errors.Is(err, hetzner.ErrNotFound) {
		t.Errorf("deleted network: %v", err)
	}
}

func TestLoadBalancer(t *testing.T) {
	srv := cloudFake(t)
	c := srv.Client()
	ctx := context.Background()
	netID := srv.Networks()[0].ID
	hc := hetzner.LBHealthCheck{Protocol: "tcp", Port: 443, Interval: 15, Timeout: 10, Retries: 3}
	lb, err := c.CreateLoadBalancer(ctx, hetzner.LoadBalancerOpts{Name: "kwerft-local", LoadBalancerType: "lb11", Location: "fsn1",
		Network: netID, PublicInterface: true, Labels: map[string]string{"kwerft.dev/cluster": "local"},
		Services: []hetzner.LBService{{Protocol: "tcp", ListenPort: 443, DestinationPort: 443, Proxyprotocol: true, HealthCheck: hc}},
		Targets:  []hetzner.LBTarget{hetzner.SelectorTarget("kwerft.dev/cluster=local")}})
	if err != nil {
		t.Fatal(err)
	}
	if lb.PublicNet.IPv4.IP == "" || !lb.AttachedTo(netID) {
		t.Fatalf("created %+v", lb)
	}
	hc.Port = 80
	if err := c.AddLoadBalancerService(ctx, lb.ID, hetzner.LBService{Protocol: "tcp", ListenPort: 80, DestinationPort: 80, Proxyprotocol: true, HealthCheck: hc}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddLoadBalancerTarget(ctx, lb.ID, hetzner.ServerTarget(1)); err != nil {
		t.Fatal(err)
	}
	var apiErr *hetzner.APIError
	if err := c.AddLoadBalancerTarget(ctx, lb.ID, hetzner.ServerTarget(1)); !errors.As(err, &apiErr) || apiErr.Code != "target_already_defined" {
		t.Errorf("same target twice: %v", err)
	}
	srv.SetTargetHealth(2, "unhealthy")
	got, err := c.GetLoadBalancer(ctx, lb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if targets, healthy := got.Health(443); targets != 2 || healthy != 1 {
		t.Errorf("health = %d/%d", healthy, targets)
	}
	if err := c.RemoveLoadBalancerTarget(ctx, lb.ID, hetzner.ServerTarget(1)); err != nil {
		t.Fatal(err)
	}
	if list, err := c.LoadBalancers(ctx, "kwerft.dev/cluster=local"); err != nil || len(list) != 1 || len(list[0].Targets) != 1 {
		t.Fatalf("LoadBalancers = %+v, %v", list, err)
	}
	if err := c.DeleteLoadBalancer(ctx, lb.ID); err != nil {
		t.Fatal(err)
	}
	if len(srv.LoadBalancers()) != 0 {
		t.Error("not deleted")
	}
}
