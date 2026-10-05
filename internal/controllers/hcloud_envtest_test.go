package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

// The Hetzner Cloud reconciler against the real API server, next to the
// running Domain and firewall reconcilers: its status fields pass the CRD
// schema, the rules' cloudFirewall states are written, the token Secret the
// Domain reconciler creates is where it reads, and a Load Balancer that
// serves moves status.publicAddresses (where DNS points) to itself.
func TestHetznerCloudOnAPIServer(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	useSettings(t, kwerftv1.ConsoleSettingsSpec{HetznerCloud: &kwerftv1.HetznerCloudSettings{
		LoadBalancer: &kwerftv1.LoadBalancerSettings{Enabled: true, Type: "lb11"}}})

	hz := hetznertest.New(t, hcToken)
	hz.EnableFirewalls()
	hz.EnableNetworks()
	hz.EnableLoadBalancers()
	netID := hz.PutNetwork(hetzner.Network{Name: "kwerft", IPRange: "10.0.0.0/16"})
	hz.AddServer(hetznertest.NewServerSummary(1, "hc-node", "fsn1", "203.0.113.10", "2001:db8:1::/64", netID, "10.0.0.2", nil))

	n := node("hc-node", "cloud", "203.0.113.10")
	if err := k8s.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(ctx, n) })

	// The Domain reconciler creates the empty token Secret; Settings (here:
	// the test) patches the token in.
	tokenSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: HCloudTokenSecret}}
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKeyFromObject(tokenSecret), tokenSecret) })
	tokenSecret.Data = map[string][]byte{HCloudTokenKey: []byte(hcToken)}
	if err := k8s.Update(ctx, tokenSecret); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tokenSecret.Data = nil
		_ = k8s.Update(ctx, tokenSecret)
	})

	rec := &HetznerCloudReconciler{Client: k8s, HetznerAPI: hz.URL, ProxyNetwork: "10.0.0.0/16"}
	eventually(t, func() error {
		_, err := rec.Reconcile(ctx, hcloudRequest)
		return err
	})

	if fws := hz.Firewalls(); len(fws) != 1 || !firewall.CloudKeepsBaseline(fws[0].Rules) {
		t.Fatalf("firewalls %+v", fws)
	}
	var http kwerftv1.FirewallRule
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: firewall.RuleHTTP}, &http); err != nil {
			return err
		}
		if http.Status.CloudFirewall != firewall.CloudApplied {
			return fmt.Errorf("http cloudFirewall %q", http.Status.CloudFirewall)
		}
		return nil
	})

	lb := hz.LoadBalancers()
	if len(lb) != 1 {
		t.Fatalf("load balancers %+v", lb)
	}
	want := []string{lb[0].PublicNet.IPv4.IP, lb[0].PublicNet.IPv6.IP}
	waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		st := s.Status.HetznerCloud
		if st == nil || st.Firewall == nil || st.LoadBalancer == nil || !st.LoadBalancer.Active {
			return fmt.Errorf("status %+v", st)
		}
		if len(st.Servers) != 1 || st.Servers[0].Node != "hc-node" {
			return fmt.Errorf("servers %+v", st.Servers)
		}
		if !slices.Equal(s.Status.PublicAddresses, want) {
			return fmt.Errorf("publicAddresses %v, want the Load Balancer's %v", s.Status.PublicAddresses, want)
		}
		return nil
	})

	// Off again: DNS goes back to the nodes at once.
	var s kwerftv1.ConsoleSettings
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		s.Spec.HetznerCloud.LoadBalancer.Enabled = false
		return k8s.Update(ctx, &s)
	})
	eventually(t, func() error {
		_, err := rec.Reconcile(ctx, hcloudRequest)
		return err
	})
	waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		if slices.Contains(s.Status.PublicAddresses, want[0]) {
			return errors.New("DNS still points at the Load Balancer")
		}
		if lb := s.Status.HetznerCloud.LoadBalancer; lb == nil || lb.State != "Draining" {
			return fmt.Errorf("load balancer %+v", lb)
		}
		return nil
	})
}
