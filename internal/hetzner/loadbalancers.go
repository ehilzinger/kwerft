// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetzner

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Load Balancers (/load_balancers). Kwerft uses TCP services with the PROXY
// protocol in front of the ingress, targeting servers over a private network.

// LBHealthCheck checks a service on every target.
type LBHealthCheck struct {
	Protocol string `json:"protocol"` // tcp | http
	Port     int    `json:"port"`
	Interval int    `json:"interval"` // seconds, 3–60
	Timeout  int    `json:"timeout"`  // seconds, 1–60
	Retries  int    `json:"retries"`  // 1–5
}

// LBService forwards one listen port to the targets.
type LBService struct {
	Protocol        string        `json:"protocol"` // tcp | http | https
	ListenPort      int           `json:"listen_port"`
	DestinationPort int           `json:"destination_port"`
	Proxyprotocol   bool          `json:"proxyprotocol"`
	HealthCheck     LBHealthCheck `json:"health_check"`
}

// LBHealth is a target's health for one listen port.
type LBHealth struct {
	ListenPort int    `json:"listen_port"`
	Status     string `json:"status"` // healthy | unhealthy | unknown
}

// LBTarget is a server, a label selector (Targets lists the servers it
// matches, in answers) or an IP.
type LBTarget struct {
	Type          string         `json:"type"` // server | label_selector | ip
	Server        *ResourceRef   `json:"server,omitempty"`
	LabelSelector *LabelSelector `json:"label_selector,omitempty"`
	UsePrivateIP  bool           `json:"use_private_ip"`
	HealthStatus  []LBHealth     `json:"health_status,omitempty"`
	Targets       []LBTarget     `json:"targets,omitempty"`
}

// ServerTarget and SelectorTarget build targets reached over the private
// network.
func ServerTarget(id int64) LBTarget {
	return LBTarget{Type: "server", Server: &ResourceRef{ID: id}, UsePrivateIP: true}
}

func SelectorTarget(selector string) LBTarget {
	return LBTarget{Type: "label_selector", LabelSelector: &LabelSelector{Selector: selector}, UsePrivateIP: true}
}

// Key identifies the target ("server:42", "label_selector:a=b").
func (t LBTarget) Key() string {
	switch {
	case t.Server != nil:
		return "server:" + strconv.FormatInt(t.Server.ID, 10)
	case t.LabelSelector != nil:
		return "label_selector:" + t.LabelSelector.Selector
	}
	return t.Type
}

func (t LBTarget) request() LBTarget {
	t.HealthStatus, t.Targets = nil, nil
	return t
}

// LBAddress is a public address of a Load Balancer.
type LBAddress struct {
	IP string `json:"ip"`
}

// LoadBalancer is a Hetzner Load Balancer.
type LoadBalancer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		Enabled bool      `json:"enabled"`
		IPv4    LBAddress `json:"ipv4"`
		IPv6    LBAddress `json:"ipv6"`
	} `json:"public_net"`
	PrivateNet       []PrivateNetRef `json:"private_net"`
	Location         LocationRef     `json:"location"`
	LoadBalancerType struct {
		Name string `json:"name"`
	} `json:"load_balancer_type"`
	Services []LBService `json:"services"`
	Targets  []LBTarget  `json:"targets"`
}

// LocationRef is the location a resource is in.
type LocationRef struct {
	Name        string `json:"name"`
	NetworkZone string `json:"network_zone"`
}

// Health counts the servers behind the Load Balancer and how many of them
// are healthy on port (a server counts once, however it is targeted).
func (lb LoadBalancer) Health(port int) (targets, healthy int) {
	state := map[string]bool{}
	var visit func(t LBTarget)
	visit = func(t LBTarget) {
		if t.Server != nil {
			key := t.Key()
			ok := false
			for _, h := range t.HealthStatus {
				if h.ListenPort == port && h.Status == "healthy" {
					ok = true
				}
			}
			state[key] = state[key] || ok
		}
		for _, sub := range t.Targets {
			visit(sub)
		}
	}
	for _, t := range lb.Targets {
		visit(t)
	}
	for _, ok := range state {
		targets++
		if ok {
			healthy++
		}
	}
	return targets, healthy
}

// AttachedTo reports whether the Load Balancer is in the network.
func (lb LoadBalancer) AttachedTo(network int64) bool {
	for _, n := range lb.PrivateNet {
		if n.Network == network {
			return true
		}
	}
	return false
}

// LoadBalancerOpts creates a Load Balancer.
type LoadBalancerOpts struct {
	Name             string            `json:"name"`
	LoadBalancerType string            `json:"load_balancer_type"`
	Location         string            `json:"location,omitempty"`
	NetworkZone      string            `json:"network_zone,omitempty"`
	Network          int64             `json:"network,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Services         []LBService       `json:"services,omitempty"`
	Targets          []LBTarget        `json:"targets,omitempty"`
	PublicInterface  bool              `json:"public_interface"`
}

// LoadBalancers lists the project's Load Balancers matching selector.
func (c *Client) LoadBalancers(ctx context.Context, selector string) ([]LoadBalancer, error) {
	var out []LoadBalancer
	for page := 1; ; page++ {
		var body struct {
			LoadBalancers []LoadBalancer `json:"load_balancers"`
			Meta          meta           `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		if err := c.do(ctx, http.MethodGet, "/load_balancers?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.LoadBalancers...)
		if body.Meta.Pagination.NextPage == nil || len(body.LoadBalancers) == 0 {
			return out, nil
		}
	}
}

// GetLoadBalancer reads one Load Balancer.
func (c *Client) GetLoadBalancer(ctx context.Context, id int64) (LoadBalancer, error) {
	var body struct {
		LoadBalancer LoadBalancer `json:"load_balancer"`
	}
	err := c.do(ctx, http.MethodGet, lbPath(id), nil, &body)
	return body.LoadBalancer, err
}

// CreateLoadBalancer creates a Load Balancer and waits until it exists.
func (c *Client) CreateLoadBalancer(ctx context.Context, opts LoadBalancerOpts) (LoadBalancer, error) {
	for i, t := range opts.Targets {
		opts.Targets[i] = t.request()
	}
	var body struct {
		LoadBalancer LoadBalancer `json:"load_balancer"`
		Action       *Action      `json:"action"`
	}
	if err := c.do(ctx, http.MethodPost, "/load_balancers", opts, &body); err != nil {
		return LoadBalancer{}, err
	}
	if body.Action != nil {
		if err := c.WaitActions(ctx, *body.Action); err != nil {
			return body.LoadBalancer, err
		}
	}
	return body.LoadBalancer, nil
}

// AddLoadBalancerService adds a service.
func (c *Client) AddLoadBalancerService(ctx context.Context, id int64, s LBService) error {
	return c.doAction(ctx, http.MethodPost, lbPath(id)+"/actions/add_service", s)
}

// UpdateLoadBalancerService changes the service on s.ListenPort.
func (c *Client) UpdateLoadBalancerService(ctx context.Context, id int64, s LBService) error {
	return c.doAction(ctx, http.MethodPost, lbPath(id)+"/actions/update_service", s)
}

// AddLoadBalancerTarget adds a target.
func (c *Client) AddLoadBalancerTarget(ctx context.Context, id int64, t LBTarget) error {
	return c.doAction(ctx, http.MethodPost, lbPath(id)+"/actions/add_target", t.request())
}

// RemoveLoadBalancerTarget removes a target.
func (c *Client) RemoveLoadBalancerTarget(ctx context.Context, id int64, t LBTarget) error {
	body := map[string]any{"type": t.Type}
	if t.Server != nil {
		body["server"] = t.Server
	}
	if t.LabelSelector != nil {
		body["label_selector"] = t.LabelSelector
	}
	return c.doAction(ctx, http.MethodPost, lbPath(id)+"/actions/remove_target", body)
}

// AttachLoadBalancerToNetwork puts the Load Balancer into a private network.
func (c *Client) AttachLoadBalancerToNetwork(ctx context.Context, id, network int64) error {
	return c.doAction(ctx, http.MethodPost, lbPath(id)+"/actions/attach_to_network", map[string]any{"network": network})
}

// DeleteLoadBalancer deletes a Load Balancer; a missing one is not an error.
func (c *Client) DeleteLoadBalancer(ctx context.Context, id int64) error {
	return notFoundIsGone(c.do(ctx, http.MethodDelete, lbPath(id), nil, nil))
}

func lbPath(id int64) string { return "/load_balancers/" + strconv.FormatInt(id, 10) }
