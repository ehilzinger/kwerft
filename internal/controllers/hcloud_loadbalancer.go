package controllers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/hetzner"
)

// The Load Balancer in front of the ingress (ConsoleSettings
// spec.hetznerCloud.loadBalancer): a Hetzner Load Balancer named like the
// Cloud Firewall, in the cluster's private network, with TCP services on 80
// and 443 that speak the PROXY protocol (Traefik accepts it from the
// private network, install.sh), targeting the cluster's Cloud servers over
// their private IPs: the label selector kwerft.dev/cluster=<cluster> plus
// the other Cloud nodes by ID.
//
// DNS moves to it only once a target passes the health check on 443
// (status.loadBalancer.active; the Domain reconciler then reports its
// addresses as status.publicAddresses, which the DNS reconciler and the
// Settings checks follow). Turning it off moves DNS back to the nodes at
// once and deletes the Load Balancer LBDrain later, when resolvers no
// longer hand out its address.

const (
	// LBDefaultType is the smallest Load Balancer (lb11, 2026-10).
	LBDefaultType = "lb11"
	// LBDrain is how long a turned-off Load Balancer keeps serving: twice
	// the TTL of Kwerft's DNS records.
	LBDrain = 2 * dnsTTL * time.Second
)

// lbServices: TCP passthrough with the PROXY protocol, TLS stays with
// Traefik (certificates from cert-manager).
func lbServices() []hetzner.LBService {
	var out []hetzner.LBService
	for _, port := range []int{80, 443} {
		out = append(out, hetzner.LBService{Protocol: "tcp", ListenPort: port, DestinationPort: port, Proxyprotocol: true,
			HealthCheck: hetzner.LBHealthCheck{Protocol: "tcp", Port: port, Interval: 15, Timeout: 10, Retries: 3}})
	}
	return out
}

func (r *HetznerCloudReconciler) syncLoadBalancer(ctx context.Context, hz *hetzner.Client, s *kwerftv1.ConsoleSettings, instance string, cloud []CloudNode) (*kwerftv1.LoadBalancerStatus, error) {
	var prev *kwerftv1.LoadBalancerStatus
	if s.Status.HetznerCloud != nil {
		prev = s.Status.HetznerCloud.LoadBalancer
	}
	existing, err := hz.LoadBalancers(ctx, r.ownedSelector(instance))
	if err != nil {
		return prev, err
	}
	slices.SortFunc(existing, func(a, b hetzner.LoadBalancer) int { return int(a.ID - b.ID) })
	spec := (*kwerftv1.LoadBalancerSettings)(nil)
	if s.Spec.HetznerCloud != nil {
		spec = s.Spec.HetznerCloud.LoadBalancer
	}

	if spec == nil || !spec.Enabled {
		if len(existing) == 0 {
			return nil, nil
		}
		since := r.now()
		if prev != nil && prev.DrainingSince != nil {
			since = prev.DrainingSince.Time
		}
		if r.now().Sub(since) >= LBDrain || prev == nil || !prev.Active && prev.DrainingSince == nil {
			// Drained, or DNS never pointed at it: delete.
			for _, lb := range existing {
				if err := hz.DeleteLoadBalancer(ctx, lb.ID); err != nil {
					st := lbStatus(lb, "Error")
					st.Message = "Could not delete the Load Balancer: " + err.Error()
					return st, err
				}
			}
			return nil, nil
		}
		st := lbStatus(existing[0], "Draining")
		st.DrainingSince = &metav1.Time{Time: since}
		st.Message = fmt.Sprintf("Turned off: DNS points at the nodes again; the Load Balancer is deleted at %s.",
			since.Add(LBDrain).UTC().Format(time.TimeOnly+" MST"))
		return st, nil
	}

	if r.ProxyNetwork == "" {
		return &kwerftv1.LoadBalancerStatus{State: "Error",
			Message: "The ingress does not accept the PROXY protocol yet. Re-run the installer on a Cloud server with a private network (Hetzner Cloud Network); it sets that up."}, nil
	}
	proxy, err := netip.ParsePrefix(r.ProxyNetwork)
	if err != nil {
		return &kwerftv1.LoadBalancerStatus{State: "Error", Message: "Invalid --hcloud-proxy-network " + r.ProxyNetwork}, nil
	}
	network, location := int64(0), ""
	for _, n := range cloud {
		for _, p := range n.Server.PrivateNet {
			if ip, err := netip.ParseAddr(p.IP); err == nil && proxy.Contains(ip) && network == 0 {
				network, location = p.Network, n.Server.Location.Name
			}
		}
	}
	if network == 0 {
		return &kwerftv1.LoadBalancerStatus{State: "Error",
			Message: "None of the cluster's Cloud servers is in the private network " + r.ProxyNetwork + "; the Load Balancer reaches the servers through it."}, nil
	}
	if spec.Location != "" {
		location = spec.Location
	}
	want := []hetzner.LBTarget{hetzner.SelectorTarget(CloudSelector(r.cluster()))}
	for _, n := range cloud {
		if !n.Labelled && slices.ContainsFunc(n.Server.PrivateNet, func(p hetzner.PrivateNetRef) bool {
			return p.Network == network
		}) {
			want = append(want, hetzner.ServerTarget(n.Server.ID))
		}
	}

	var lb hetzner.LoadBalancer
	if len(existing) == 0 {
		typ := spec.Type
		if typ == "" {
			typ = LBDefaultType
		}
		lb, err = hz.CreateLoadBalancer(ctx, hetzner.LoadBalancerOpts{Name: r.resourceName(instance), LoadBalancerType: typ,
			Location: location, Network: network, Labels: r.ownedLabels(instance), Services: lbServices(), Targets: want,
			PublicInterface: true})
		var apiErr *hetzner.APIError
		if errors.As(err, &apiErr) && apiErr.Code == "uniqueness_error" {
			return &kwerftv1.LoadBalancerStatus{State: "Error", Name: r.resourceName(instance),
				Message: "The project already has a Load Balancer named " + r.resourceName(instance) + " that Kwerft did not create. Rename or delete it in the Hetzner Console."}, nil
		}
		if err != nil {
			return &kwerftv1.LoadBalancerStatus{State: "Error", Message: "Could not create the Load Balancer: " + err.Error()}, err
		}
	} else {
		lb = existing[0]
		if err := r.convergeLoadBalancer(ctx, hz, lb, network, want); err != nil {
			st := lbStatus(lb, "Error")
			st.Active = prev != nil && prev.Active
			st.Message = err.Error()
			return st, err
		}
	}
	if fresh, err := hz.GetLoadBalancer(ctx, lb.ID); err == nil {
		lb = fresh
	}
	st := lbStatus(lb, "Waiting")
	targets, healthy := lb.Health(443)
	st.Targets, st.HealthyTargets = int32(targets), int32(healthy)
	// Once DNS points at it, it stays there (no flapping between the Load
	// Balancer and the nodes); turning it off is the way back.
	st.Active = (healthy > 0 || prev != nil && prev.Active) && st.IPv4 != ""
	switch {
	case st.Active && healthy == 0:
		st.State = "Active"
		st.Message = "No server passes the health check on port 443."
	case st.Active:
		st.State = "Active"
	case targets == 0:
		st.Message = "No targets yet: the cluster's Cloud servers are added as they appear."
	default:
		st.Message = "Waiting for a server to pass the health check on port 443; DNS moves to the Load Balancer then."
	}
	if spec.Type != "" && lb.LoadBalancerType.Name != spec.Type {
		st.Message = "The Load Balancer is of type " + lb.LoadBalancerType.Name + "; change it in the Hetzner Console (Kwerft does not resize it). " + st.Message
	}
	return st, nil
}

// convergeLoadBalancer puts the network, the services and the targets back
// as Kwerft wants them.
func (r *HetznerCloudReconciler) convergeLoadBalancer(ctx context.Context, hz *hetzner.Client, lb hetzner.LoadBalancer, network int64, want []hetzner.LBTarget) error {
	if !lb.AttachedTo(network) {
		if err := hz.AttachLoadBalancerToNetwork(ctx, lb.ID, network); err != nil {
			return fmt.Errorf("could not attach the Load Balancer to the private network: %w", err)
		}
	}
	for _, svc := range lbServices() {
		i := slices.IndexFunc(lb.Services, func(o hetzner.LBService) bool { return o.ListenPort == svc.ListenPort })
		switch {
		case i < 0:
			if err := hz.AddLoadBalancerService(ctx, lb.ID, svc); err != nil {
				return fmt.Errorf("could not add the service on port %d: %w", svc.ListenPort, err)
			}
		case lb.Services[i] != svc:
			if err := hz.UpdateLoadBalancerService(ctx, lb.ID, svc); err != nil {
				return fmt.Errorf("could not update the service on port %d: %w", svc.ListenPort, err)
			}
		}
	}
	for _, t := range want {
		if !slices.ContainsFunc(lb.Targets, func(o hetzner.LBTarget) bool { return o.Key() == t.Key() }) {
			if err := hz.AddLoadBalancerTarget(ctx, lb.ID, t); err != nil {
				return fmt.Errorf("could not add the target %s: %w", t.Key(), err)
			}
		}
	}
	for _, t := range lb.Targets {
		if (t.Type == "server" || t.Type == "label_selector") &&
			!slices.ContainsFunc(want, func(o hetzner.LBTarget) bool { return o.Key() == t.Key() }) {
			if err := hz.RemoveLoadBalancerTarget(ctx, lb.ID, t); err != nil && !errors.Is(err, hetzner.ErrNotFound) {
				return fmt.Errorf("could not remove the target %s: %w", t.Key(), err)
			}
		}
	}
	return nil
}

func lbStatus(lb hetzner.LoadBalancer, state string) *kwerftv1.LoadBalancerStatus {
	return &kwerftv1.LoadBalancerStatus{State: state, ID: lb.ID, Name: lb.Name, Type: lb.LoadBalancerType.Name,
		Location: lb.Location.Name, IPv4: lb.PublicNet.IPv4.IP, IPv6: lb.PublicNet.IPv6.IP}
}

// LoadBalancerAddresses are the addresses DNS points at while the Load
// Balancer serves (status.loadBalancer.active), else nil.
func LoadBalancerAddresses(s *kwerftv1.ConsoleSettings) []string {
	if s == nil || s.Status.HetznerCloud == nil || s.Status.HetznerCloud.LoadBalancer == nil {
		return nil
	}
	lb := s.Status.HetznerCloud.LoadBalancer
	if !lb.Active || lb.State == "Draining" {
		return nil
	}
	var out []string
	for _, a := range []string{lb.IPv4, lb.IPv6} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}
