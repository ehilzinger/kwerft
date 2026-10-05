package hetzner

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"
)

// Server is a Cloud server.
type Server struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // initializing | starting | running | stopping | off | deleting | migrating | rebuilding | unknown
	// Created is when the server was ordered.
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 *struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
		IPv6 *struct {
			IP string `json:"ip"`
		} `json:"ipv6"`
	} `json:"public_net"`
	PrivateNet []struct {
		Network int64  `json:"network"`
		IP      string `json:"ip"`
	} `json:"private_net"`
	ServerType     ServerType      `json:"server_type"`
	Location       Location        `json:"location"`
	Image          *Image          `json:"image"`
	PlacementGroup *PlacementGroup `json:"placement_group"`
}

// Server statuses Kwerft acts on.
const (
	ServerRunning  = "running"
	ServerDeleting = "deleting"
)

// PublicIPv4 is the server's public IPv4 address, or "".
func (s Server) PublicIPv4() string {
	if s.PublicNet.IPv4 == nil {
		return ""
	}
	return s.PublicNet.IPv4.IP
}

// PrivateIP is the server's address in the network (any network when 0).
func (s Server) PrivateIP(network int64) string {
	for _, n := range s.PrivateNet {
		if network == 0 || n.Network == network {
			return n.IP
		}
	}
	return ""
}

// CreateServerOpts is a new server.
type CreateServerOpts struct {
	Name       string `json:"name"`
	ServerType string `json:"server_type"`
	Location   string `json:"location,omitempty"`
	Image      string `json:"image"`
	// PlacementGroup ID; 0 for none.
	PlacementGroup int64             `json:"placement_group,omitempty"`
	SSHKeys        []string          `json:"ssh_keys,omitempty"`
	Networks       []int64           `json:"networks,omitempty"`
	UserData       string            `json:"user_data,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// MaxUserData is the Cloud API's limit for cloud-init user data.
const MaxUserData = 32 << 10

// Servers lists servers matching labelSelector ("" for all).
func (c *Client) Servers(ctx context.Context, labelSelector string) ([]Server, error) {
	return pagedList[Server](ctx, c, "/servers", "servers", selector(labelSelector))
}

// Server reads one server; ErrNotFound when it is gone.
func (c *Client) Server(ctx context.Context, id int64) (Server, error) {
	var body struct {
		Server Server `json:"server"`
	}
	err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &body)
	return body.Server, err
}

// CreateServer orders a server; it starts once created. The returned
// action tracks the creation.
func (c *Client) CreateServer(ctx context.Context, opts CreateServerOpts) (Server, Action, error) {
	if len(opts.UserData) > MaxUserData {
		return Server{}, Action{}, errors.New("cloud-init user data is larger than 32 KiB")
	}
	var body struct {
		Server Server `json:"server"`
		Action Action `json:"action"`
	}
	err := c.do(ctx, http.MethodPost, "/servers", opts, &body)
	return body.Server, body.Action, err
}

// DeleteServer deletes a server; a missing one is not an error. Callers
// must only delete servers carrying LabelCluster and LabelPool.
func (c *Client) DeleteServer(ctx context.Context, id int64) error {
	err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// SetServerLabels replaces a server's labels.
func (c *Client) SetServerLabels(ctx context.Context, id int64, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, "/servers/"+strconv.FormatInt(id, 10), map[string]any{"labels": labels}, nil)
}
