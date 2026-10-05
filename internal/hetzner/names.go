package hetzner

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Names and labels of the Cloud resources Kwerft creates for a cluster,
// shared by everything that creates or finds them (node pools, the Cloud
// Firewall sync, load balancers, volumes): see docs/phase5.md.
const (
	// LabelCluster and LabelPool are on every server Kwerft creates;
	// Kwerft only ever deletes servers that carry both (docs/phase5.md ›
	// Security).
	LabelCluster = "kwerft.dev/cluster"
	LabelPool    = "kwerft.dev/pool"
	// LabelRole is the pool's role (worker, control-plane, builds).
	LabelRole = "kwerft.dev/role"
	// LabelBootstrap marks the server that bootstraps a new cluster (the
	// first control-plane server, installed in agent mode).
	LabelBootstrap = "kwerft.dev/bootstrap"
	// LabelSSHKey marks SSH keys to put on new servers; without any key so
	// marked, every key of the project is used.
	LabelSSHKey = "kwerft.dev/ssh-key"
)

// NetworkName is the name of a cluster's private Cloud Network, which every
// Cloud server of the cluster is attached to. W1 (Cloud integrations)
// creates it; node pools find it by LabelCluster or, failing that, by this
// name (a network made by hand for the local cluster can be labelled).
func NetworkName(cluster string) string { return "kwerft-" + cluster }

// PlacementGroupName is the spread placement group of a node pool.
func PlacementGroupName(cluster, pool string) string { return "kwerft-" + cluster + "-" + pool }

// NetworkRef is what node pools need of a cluster's network.
type NetworkRef struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	IPRange string `json:"ip_range"`
}

// ClusterNetwork finds the cluster's private network: the one labelled
// kwerft.dev/cluster=<cluster>, else the one named NetworkName(cluster).
// ErrNotFound when there is none.
func (c *Client) ClusterNetwork(ctx context.Context, cluster string) (NetworkRef, error) {
	for _, q := range []url.Values{
		{"label_selector": {LabelCluster + "=" + cluster}},
		{"name": {NetworkName(cluster)}},
	} {
		var body struct {
			Networks []NetworkRef `json:"networks"`
		}
		if err := c.do(ctx, http.MethodGet, "/networks?"+q.Encode(), nil, &body); err != nil {
			return NetworkRef{}, err
		}
		if len(body.Networks) > 0 {
			return body.Networks[0], nil
		}
	}
	return NetworkRef{}, ErrNotFound
}

// ValidServerName reports whether s is usable as a Cloud server name, which must
// be a valid hostname (RFC 1123): the node's name in Kubernetes too.
func ValidServerName(s string) bool {
	if s == "" || len(s) > 63 || strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}
