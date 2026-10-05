package hetzner

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
)

// What a Cloud API token's project holds, as far as Kwerft's integrations
// need it: which servers are the cluster's nodes (by address), and whether
// the token may change things. The full server API (create, delete, types,
// images) is the node pools' (servers.go).

// ServerSummary is the part of a server (GET /servers) the firewall and
// Load Balancer integrations need.
type ServerSummary struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
		IPv6 *struct {
			IP string `json:"ip"` // a /64 network
		} `json:"ipv6"`
	} `json:"public_net"`
	PrivateNet []PrivateNetRef `json:"private_net"`
	Location   LocationRef     `json:"location"`
}

// PrivateNetRef is a server's or Load Balancer's address in a network.
type PrivateNetRef struct {
	Network int64  `json:"network"`
	IP      string `json:"ip"`
}

// HasAddress reports whether addr is one of the server's public addresses:
// its IPv4, or inside its IPv6 network. Private addresses do not count:
// every Cloud Network of a project can hand out the same ones (10.0.0.2 in
// each), so they name no server (a test cluster once matched a production
// server of the token's project that way).
func (s ServerSummary) HasAddress(addr netip.Addr) bool {
	addr = addr.Unmap()
	if s.PublicNet.IPv4 != nil {
		if a, err := netip.ParseAddr(s.PublicNet.IPv4.IP); err == nil && a == addr {
			return true
		}
	}
	if s.PublicNet.IPv6 != nil {
		if p, err := netip.ParsePrefix(s.PublicNet.IPv6.IP); err == nil && p.Contains(addr) {
			return true
		}
	}
	return false
}

// ServerSummaries lists the project's servers matching selector (all when
// empty).
func (c *Client) ServerSummaries(ctx context.Context, selector string) ([]ServerSummary, error) {
	var out []ServerSummary
	for page := 1; ; page++ {
		var body struct {
			Servers []ServerSummary `json:"servers"`
			Meta    meta            `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		if err := c.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Servers...)
		if body.Meta.Pagination.NextPage == nil || len(body.Servers) == 0 {
			return out, nil
		}
	}
}

// ErrReadOnly: the token may read but not change the project.
var ErrReadOnly = errors.New("read-only token")

// ProbeWrite tells a Read & Write token from a Read-only one without
// changing anything: it asks to create a firewall without a name, which a
// writing token gets refused as invalid input (422) and a read-only one as
// forbidden (403). Should Hetzner ever accept it, the firewall is deleted
// again at once.
func (c *Client) ProbeWrite(ctx context.Context) error {
	var body struct {
		Firewall *Firewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodPost, "/firewalls", map[string]any{"name": ""}, &body)
	var apiErr *APIError
	switch {
	case errors.Is(err, ErrForbidden):
		return ErrReadOnly
	case errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnprocessableEntity || apiErr.Status == http.StatusBadRequest):
		return nil
	case err != nil:
		return err
	}
	if body.Firewall != nil && body.Firewall.ID != 0 {
		return c.DeleteFirewall(ctx, body.Firewall.ID)
	}
	return nil
}
