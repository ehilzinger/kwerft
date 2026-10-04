package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClientIPTrustsXRealIPOnlyFromTraefik(t *testing.T) {
	traefik := netip.MustParseAddr("10.42.0.17") // a node's cilium_host address
	trusted := func(a netip.Addr) bool { return a == traefik }
	for _, c := range []struct {
		name, peer, header, want string
	}{
		{"from Traefik", "10.42.0.17:41234", "203.0.113.9", "203.0.113.9"},
		{"from Traefik, IPv6 client", "10.42.0.17:41234", "2001:db8::7", "2001:db8::7"},
		{"from Traefik, garbage header", "10.42.0.17:41234", "203.0.113.9, 10.0.0.1", "10.42.0.17"},
		{"from Traefik, no header", "10.42.0.17:41234", "", "10.42.0.17"},
		{"a pod spoofing it", "10.42.0.99:5555", "203.0.113.9", "10.42.0.99"},
		{"a public client spoofing it", "198.51.100.4:5555", "127.0.0.1", "198.51.100.4"},
		{"loopback (dev proxy, tests)", "127.0.0.1:5555", "203.0.113.9", "203.0.113.9"},
		{"v4-mapped Traefik", "[::ffff:10.42.0.17]:41234", "203.0.113.9", "203.0.113.9"},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = c.peer
		if c.header != "" {
			r.Header.Set("X-Real-Ip", c.header)
		}
		if got := resolveClientIP(r, trusted); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
	// Without a trust function only loopback is believed.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.42.0.17:1"
	r.Header.Set("X-Real-Ip", "203.0.113.9")
	if got := resolveClientIP(r, nil); got != "10.42.0.17" {
		t.Errorf("no trust function: %s", got)
	}
	// The middleware stores the decision; ClientIP reads it.
	var seen string
	h := withClientIP(trusted, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = ClientIP(r) }))
	r = httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.42.0.17:1"
	r.Header.Set("X-Real-Ip", "203.0.113.10")
	h.ServeHTTP(httptest.NewRecorder(), r)
	if seen != "203.0.113.10" {
		t.Errorf("ClientIP via middleware: %s", seen)
	}
}

func TestNodePeersAreNodeAndCiliumAddresses(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Addresses: []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: "10.0.0.2"},
		{Type: corev1.NodeExternalIP, Address: "203.0.113.24"},
		{Type: corev1.NodeHostName, Address: "n1"},
	}}}
	cn := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2", "kind": "CiliumNode", "metadata": map[string]any{"name": "n1"},
		"spec": map[string]any{"addresses": []any{
			map[string]any{"type": "InternalIP", "ip": "10.0.0.2"},
			map[string]any{"type": "CiliumInternalIP", "ip": "10.42.0.17"},
			map[string]any{"type": "CiliumInternalIP", "ip": "fd00::17"},
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, cn).Build()
	peers := NewNodePeers(c, nil)
	if peers.Trusted(netip.MustParseAddr("10.42.0.17")) {
		t.Fatal("trusted before the first refresh")
	}
	if err := peers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for addr, want := range map[string]bool{
		"10.0.0.2": true, "203.0.113.24": true, "10.42.0.17": true, "fd00::17": true,
		"::ffff:10.42.0.17": true, "10.42.0.18": false, "198.51.100.1": false,
	} {
		if got := peers.Trusted(netip.MustParseAddr(addr)); got != want {
			t.Errorf("Trusted(%s) = %v", addr, got)
		}
	}
	// Without Cilium the node addresses alone remain.
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	peers = NewNodePeers(c, nil)
	if err := peers.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !peers.Trusted(netip.MustParseAddr("10.0.0.2")) || peers.Trusted(netip.MustParseAddr("10.42.0.17")) {
		t.Error("without CiliumNodes")
	}
}
