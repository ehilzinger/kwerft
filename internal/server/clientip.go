package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The client's address, for rate limits, the audit log and (Phase 4) the
// server firewall's lock-out check.
//
// Traefik runs as a hostNetwork DaemonSet and sets X-Real-Ip (overwriting
// whatever the client sent) on every request it proxies. Anyone else who can
// reach the console's pod — another pod, in principle — could send the
// header too, so it is believed only when the TCP peer is Traefik.
//
// What the peer looks like: Traefik lives in a node's host network and
// connects to the console's pod IP. Cilium routes the pod CIDR through
// cilium_host with that interface's address as the source, so the console
// sees the "CiliumInternalIP" (router IP) of the node Traefik runs on — on
// the same node and, through the tunnel or WireGuard, from other nodes. Its
// address lies inside the pod CIDR but belongs to no pod. Depending on the
// datapath the peer can also be a node's InternalIP. Trusted peers are
// therefore exactly the addresses of the cluster's nodes: Node
// status.addresses and CiliumNode spec.addresses (which include the router
// IP). Loopback is trusted too (a process in the console's own pod: tests,
// the Vite proxy in development). Pod IPs never are.

type ctxClientIP struct{}

// ClientIP is the address the request came from: X-Real-Ip when the peer is
// a trusted proxy (Traefik), otherwise the TCP peer. Everything that keys on
// the client — rate limits, the audit log, the firewall's lock-out check —
// uses it.
func ClientIP(r *http.Request) string {
	if ip, ok := r.Context().Value(ctxClientIP{}).(string); ok {
		return ip
	}
	return resolveClientIP(r, nil)
}

// clientIP is ClientIP; the shorter name predates Phase 4.
func clientIP(r *http.Request) string { return ClientIP(r) }

func peerAddr(r *http.Request) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host
	}
	return addr.Unmap(), addr.Unmap().String()
}

// resolveClientIP decides once per request. trusted nil trusts loopback only.
func resolveClientIP(r *http.Request, trusted func(netip.Addr) bool) string {
	peer, peerText := peerAddr(r)
	if !peer.IsValid() {
		return peerText
	}
	if peer.IsLoopback() || (trusted != nil && trusted(peer)) {
		if v := strings.TrimSpace(r.Header.Get("X-Real-Ip")); v != "" {
			if ip, err := netip.ParseAddr(v); err == nil {
				return ip.Unmap().String()
			}
		}
	}
	return peerText
}

// withClientIP stores the client address in the request context for
// ClientIP.
func withClientIP(trusted func(netip.Addr) bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := resolveClientIP(r, trusted)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxClientIP{}, ip)))
	})
}

// NodePeers knows the addresses of the cluster's nodes, the only peers whose
// X-Real-Ip is believed (see above). It refreshes in the background; until
// the first refresh nothing but loopback is trusted, which errs on the safe
// side (the peer address is used).
type NodePeers struct {
	reader client.Reader
	log    *slog.Logger
	every  time.Duration
	addrs  atomic.Pointer[map[netip.Addr]bool]
}

// NewNodePeers reads Nodes and CiliumNodes through reader (the console's own
// identity; it may list nodes, and ciliumnodes since Phase 4).
func NewNodePeers(reader client.Reader, log *slog.Logger) *NodePeers {
	return &NodePeers{reader: reader, log: log, every: 30 * time.Second}
}

// Trusted reports whether addr is a node address.
func (n *NodePeers) Trusted(addr netip.Addr) bool {
	m := n.addrs.Load()
	return m != nil && (*m)[addr.Unmap()]
}

var ciliumNodeList = schema.GroupVersionKind{Group: "cilium.io", Version: "v2", Kind: "CiliumNodeList"}

// Refresh reads the node addresses once.
func (n *NodePeers) Refresh(ctx context.Context) error {
	set := map[netip.Addr]bool{}
	add := func(s string) {
		if a, err := netip.ParseAddr(s); err == nil {
			set[a.Unmap()] = true
		}
	}
	var nodes corev1.NodeList
	if err := n.reader.List(ctx, &nodes); err != nil {
		return err
	}
	for _, node := range nodes.Items {
		for _, a := range node.Status.Addresses {
			if a.Type == corev1.NodeInternalIP || a.Type == corev1.NodeExternalIP {
				add(a.Address)
			}
		}
	}
	// Cilium's router IPs. Without Cilium (development, tests) there are
	// none, and the node addresses alone remain.
	cn := &unstructured.UnstructuredList{}
	cn.SetGroupVersionKind(ciliumNodeList)
	if err := n.reader.List(ctx, cn); err == nil {
		for _, item := range cn.Items {
			list, _, _ := unstructured.NestedSlice(item.Object, "spec", "addresses")
			for _, raw := range list {
				if m, ok := raw.(map[string]any); ok {
					if ip, ok := m["ip"].(string); ok {
						add(ip)
					}
				}
			}
		}
	} else if n.log != nil {
		n.log.Debug("no CiliumNodes for trusted proxy addresses", "err", err)
	}
	n.addrs.Store(&set)
	return nil
}

// Run refreshes until ctx ends.
func (n *NodePeers) Run(ctx context.Context) {
	t := time.NewTicker(n.every)
	defer t.Stop()
	for {
		if err := n.Refresh(ctx); err != nil && n.log != nil && ctx.Err() == nil {
			n.log.Warn("cannot read node addresses; X-Real-Ip is ignored until it works", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
