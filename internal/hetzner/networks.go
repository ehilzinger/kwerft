package hetzner

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
)

// Networks (/networks): private networks between Cloud servers, Load
// Balancers and (through a vSwitch subnet) dedicated servers. Traffic on
// them is not filtered by Cloud Firewalls.

// Subnet is one subnet of a network.
type Subnet struct {
	Type        string `json:"type"` // cloud | server | vswitch
	IPRange     string `json:"ip_range,omitempty"`
	NetworkZone string `json:"network_zone"` // eu-central, us-east, ...
	Gateway     string `json:"gateway,omitempty"`
	VSwitchID   int64  `json:"vswitch_id,omitempty"`
}

// Route sends a destination through a gateway inside the network.
type Route struct {
	Destination string `json:"destination"`
	Gateway     string `json:"gateway"`
}

// Network is a private network.
type Network struct {
	ID            int64             `json:"id"`
	Name          string            `json:"name"`
	IPRange       string            `json:"ip_range"`
	Subnets       []Subnet          `json:"subnets"`
	Routes        []Route           `json:"routes"`
	Servers       []int64           `json:"servers"`
	LoadBalancers []int64           `json:"load_balancers"`
	Labels        map[string]string `json:"labels"`
	// ExposeRoutesToVSwitch makes the routes reachable from a vSwitch
	// subnet (dedicated servers).
	ExposeRoutesToVSwitch bool `json:"expose_routes_to_vswitch"`
}

// NetworkOpts creates a network.
type NetworkOpts struct {
	Name                  string            `json:"name"`
	IPRange               string            `json:"ip_range"`
	Labels                map[string]string `json:"labels,omitempty"`
	Subnets               []Subnet          `json:"subnets,omitempty"`
	ExposeRoutesToVSwitch bool              `json:"expose_routes_to_vswitch,omitempty"`
}

// Networks lists the project's networks matching selector (all when empty).
func (c *Client) Networks(ctx context.Context, selector string) ([]Network, error) {
	var out []Network
	for page := 1; ; page++ {
		var body struct {
			Networks []Network `json:"networks"`
			Meta     meta      `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if selector != "" {
			q.Set("label_selector", selector)
		}
		if err := c.do(ctx, http.MethodGet, "/networks?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Networks...)
		if body.Meta.Pagination.NextPage == nil || len(body.Networks) == 0 {
			return out, nil
		}
	}
}

// GetNetwork reads one network.
func (c *Client) GetNetwork(ctx context.Context, id int64) (Network, error) {
	var body struct {
		Network Network `json:"network"`
	}
	err := c.do(ctx, http.MethodGet, networkPath(id), nil, &body)
	return body.Network, err
}

// CreateNetwork creates a network (no action to wait for).
func (c *Client) CreateNetwork(ctx context.Context, opts NetworkOpts) (Network, error) {
	var body struct {
		Network Network `json:"network"`
	}
	err := c.do(ctx, http.MethodPost, "/networks", opts, &body)
	return body.Network, err
}

// AddSubnet adds a subnet and waits for it.
func (c *Client) AddSubnet(ctx context.Context, id int64, s Subnet) error {
	return c.doAction(ctx, http.MethodPost, networkPath(id)+"/actions/add_subnet", s)
}

// DeleteNetwork deletes a network; a missing one is not an error.
func (c *Client) DeleteNetwork(ctx context.Context, id int64) error {
	return notFoundIsGone(c.do(ctx, http.MethodDelete, networkPath(id), nil, nil))
}

func networkPath(id int64) string { return "/networks/" + strconv.FormatInt(id, 10) }
