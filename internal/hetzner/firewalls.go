package hetzner

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Cloud Firewalls (/firewalls): stateful packet filters on the public
// interface of Cloud servers. Inbound traffic not matched by an "in" rule is
// dropped; outbound is allowed without "out" rules. Private-network traffic
// is never filtered. Limits (2026-10): 50 rules per firewall, 100 CIDRs per
// rule, 5 firewalls per server.

// Firewall limits of the Cloud API.
const (
	MaxFirewallRules      = 50
	MaxFirewallRuleBlocks = 100
)

// FirewallRule is one rule. Port is "22", a range "1024-5000", or empty
// (protocols without ports).
type FirewallRule struct {
	Direction      string   `json:"direction"` // in | out
	SourceIPs      []string `json:"source_ips,omitempty"`
	DestinationIPs []string `json:"destination_ips,omitempty"`
	Protocol       string   `json:"protocol"` // tcp | udp | icmp | esp | gre
	Port           string   `json:"port,omitempty"`
}

// ResourceRef names a resource by ID.
type ResourceRef struct {
	ID int64 `json:"id"`
}

// LabelSelector selects resources by label, e.g. "kwerft.dev/cluster=local".
type LabelSelector struct {
	Selector string `json:"selector"`
}

// FirewallResource is what a firewall applies to: a server, or every server
// matching a label selector (AppliedToResources lists those, in answers).
type FirewallResource struct {
	Type               string             `json:"type"` // server | label_selector
	Server             *ResourceRef       `json:"server,omitempty"`
	LabelSelector      *LabelSelector     `json:"label_selector,omitempty"`
	AppliedToResources []FirewallResource `json:"applied_to_resources,omitempty"`
}

// ServerResource and SelectorResource build FirewallResources.
func ServerResource(id int64) FirewallResource {
	return FirewallResource{Type: "server", Server: &ResourceRef{ID: id}}
}

func SelectorResource(selector string) FirewallResource {
	return FirewallResource{Type: "label_selector", LabelSelector: &LabelSelector{Selector: selector}}
}

// Key identifies the resource ("server:42", "label_selector:a=b").
func (r FirewallResource) Key() string {
	switch {
	case r.Server != nil:
		return "server:" + strconv.FormatInt(r.Server.ID, 10)
	case r.LabelSelector != nil:
		return "label_selector:" + r.LabelSelector.Selector
	}
	return r.Type
}

// request drops the answer-only list for a request body.
func (r FirewallResource) request() FirewallResource {
	r.AppliedToResources = nil
	return r
}

// Firewall is a Cloud Firewall.
type Firewall struct {
	ID        int64              `json:"id"`
	Name      string             `json:"name"`
	Labels    map[string]string  `json:"labels"`
	Rules     []FirewallRule     `json:"rules"`
	AppliedTo []FirewallResource `json:"applied_to"`
}

// Servers counts the servers the firewall applies to, directly or through
// a label selector.
func (f Firewall) Servers() int {
	seen := map[int64]bool{}
	for _, r := range f.AppliedTo {
		if r.Server != nil {
			seen[r.Server.ID] = true
		}
		for _, s := range r.AppliedToResources {
			if s.Server != nil {
				seen[s.Server.ID] = true
			}
		}
	}
	return len(seen)
}

// Firewalls lists the project's firewalls matching selector (all when empty).
func (c *Client) Firewalls(ctx context.Context, selector string) ([]Firewall, error) {
	var out []Firewall
	for page := 1; ; page++ {
		var body struct {
			Firewalls []Firewall `json:"firewalls"`
			Meta      meta       `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		if err := c.do(ctx, http.MethodGet, "/firewalls?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Firewalls...)
		if body.Meta.Pagination.NextPage == nil || len(body.Firewalls) == 0 {
			return out, nil
		}
	}
}

// GetFirewall reads one firewall.
func (c *Client) GetFirewall(ctx context.Context, id int64) (Firewall, error) {
	var body struct {
		Firewall Firewall `json:"firewall"`
	}
	err := c.do(ctx, http.MethodGet, firewallPath(id), nil, &body)
	return body.Firewall, err
}

// CreateFirewall creates a firewall with its rules and resources, and waits
// until it is applied.
func (c *Client) CreateFirewall(ctx context.Context, name string, labels map[string]string, rules []FirewallRule, applyTo []FirewallResource) (Firewall, error) {
	req := map[string]any{"name": name, "labels": labels, "rules": listOrEmpty(rules)}
	if len(applyTo) > 0 {
		res := make([]FirewallResource, 0, len(applyTo))
		for _, r := range applyTo {
			res = append(res, r.request())
		}
		req["apply_to"] = res
	}
	var body struct {
		Firewall Firewall `json:"firewall"`
		Actions  []Action `json:"actions"`
	}
	if err := c.do(ctx, http.MethodPost, "/firewalls", req, &body); err != nil {
		return Firewall{}, err
	}
	return body.Firewall, c.WaitActions(ctx, body.Actions...)
}

// SetFirewallRules replaces every rule of the firewall and waits until the
// servers have them.
func (c *Client) SetFirewallRules(ctx context.Context, id int64, rules []FirewallRule) error {
	return c.doAction(ctx, http.MethodPost, firewallPath(id)+"/actions/set_rules", map[string]any{"rules": listOrEmpty(rules)})
}

// ApplyFirewall applies the firewall to more resources.
func (c *Client) ApplyFirewall(ctx context.Context, id int64, resources ...FirewallResource) error {
	return c.firewallResources(ctx, id, "apply_to_resources", "apply_to", resources)
}

// RemoveFirewall removes the firewall from resources.
func (c *Client) RemoveFirewall(ctx context.Context, id int64, resources ...FirewallResource) error {
	return c.firewallResources(ctx, id, "remove_from_resources", "remove_from", resources)
}

func (c *Client) firewallResources(ctx context.Context, id int64, action, field string, resources []FirewallResource) error {
	if len(resources) == 0 {
		return nil
	}
	res := make([]FirewallResource, 0, len(resources))
	for _, r := range resources {
		res = append(res, r.request())
	}
	return c.doAction(ctx, http.MethodPost, firewallPath(id)+"/actions/"+action, map[string]any{field: res})
}

// SetFirewallLabels replaces the firewall's labels.
func (c *Client) SetFirewallLabels(ctx context.Context, id int64, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, firewallPath(id), map[string]any{"labels": labels}, nil)
}

// DeleteFirewall deletes a firewall that applies to nothing any more; a
// missing one is not an error.
func (c *Client) DeleteFirewall(ctx context.Context, id int64) error {
	return notFoundIsGone(c.do(ctx, http.MethodDelete, firewallPath(id), nil, nil))
}

func firewallPath(id int64) string { return "/firewalls/" + strconv.FormatInt(id, 10) }

func listOrEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func notFoundIsGone(err error) error {
	if err == ErrNotFound {
		return nil
	}
	return err
}
