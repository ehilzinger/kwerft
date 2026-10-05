package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// RobotAPI is the Hetzner Robot webservice (dedicated servers, vSwitches).
const RobotAPI = "https://robot-ws.your-server.de"

// RobotClient talks to the Robot webservice with a webservice user (Robot ›
// Settings › Web service and app settings). Kwerft never stores these
// credentials: the owner gives them for the one action that needs them
// (coupling a vSwitch to a cluster's Cloud Network).
type RobotClient struct {
	User, Password string
	// Base defaults to RobotAPI.
	Base string
	// HTTP defaults to a client with a 15 s timeout.
	HTTP *http.Client
}

// VSwitch is a Robot vSwitch: a VLAN between dedicated servers, which a
// Cloud Network subnet of type vswitch connects to Cloud servers.
type VSwitch struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	VLAN         int              `json:"vlan"`
	Cancelled    bool             `json:"cancelled"`
	Server       []VSwitchServer  `json:"server"`
	CloudNetwork []VSwitchNetwork `json:"cloud_network"`
}

// VSwitchServer is a dedicated server on a vSwitch.
type VSwitchServer struct {
	ServerIP     string `json:"server_ip"`
	ServerNumber int64  `json:"server_number"`
	Status       string `json:"status"` // ready | in process | failed
}

// VSwitchNetwork is a Cloud Network a vSwitch is coupled to.
type VSwitchNetwork struct {
	ID      int64  `json:"id"`
	IP      string `json:"ip"`
	Mask    int    `json:"mask"`
	Gateway string `json:"gateway"`
}

// VSwitches lists the account's vSwitches (without servers).
func (c *RobotClient) VSwitches(ctx context.Context) ([]VSwitch, error) {
	var out []VSwitch
	err := c.do(ctx, http.MethodGet, "/vswitch", nil, &out)
	return out, err
}

// VSwitch reads one vSwitch with its servers and Cloud Networks.
func (c *RobotClient) VSwitch(ctx context.Context, id int64) (VSwitch, error) {
	var out VSwitch
	err := c.do(ctx, http.MethodGet, "/vswitch/"+strconv.FormatInt(id, 10), nil, &out)
	return out, err
}

// CreateVSwitch creates a vSwitch on a VLAN (4000–4091).
func (c *RobotClient) CreateVSwitch(ctx context.Context, name string, vlan int) (VSwitch, error) {
	var out VSwitch
	err := c.do(ctx, http.MethodPost, "/vswitch", url.Values{"name": {name}, "vlan": {strconv.Itoa(vlan)}}, &out)
	return out, err
}

// AddVSwitchServers connects dedicated servers (main IPv4 or server number)
// to the vSwitch.
func (c *RobotClient) AddVSwitchServers(ctx context.Context, id int64, servers []string) error {
	return c.do(ctx, http.MethodPost, "/vswitch/"+strconv.FormatInt(id, 10)+"/server", url.Values{"server[]": servers}, nil)
}

func (c *RobotClient) do(ctx context.Context, method, path string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	base := c.Base
	if base == "" {
		base = RobotAPI
	}
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(c.User, c.Password)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	hc := c.HTTP
	if hc == nil {
		hc = defaultHTTP
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	limited := http.MaxBytesReader(nil, res.Body, 4<<20)
	if res.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&e)
		switch {
		case res.StatusCode == http.StatusUnauthorized:
			return ErrTokenRejected
		case res.StatusCode == http.StatusForbidden && e.Error.Code == "RATE_LIMIT_EXCEEDED":
			return &RateLimitError{Reset: time.Now().Add(time.Hour)}
		case res.StatusCode == http.StatusNotFound && (e.Error.Code == "NOT_FOUND" || e.Error.Code == ""):
			return ErrNotFound
		}
		return &APIError{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("Hetzner Robot API: %w", err)
	}
	return nil
}

// AddVSwitchSubnet connects a vSwitch to a Cloud Network: a subnet of type
// vswitch in the network zone of the cluster's servers. Cloud servers then
// reach the dedicated servers on the vSwitch's VLAN through the network's
// gateway (the dedicated servers need a route to the network's range).
func (c *Client) AddVSwitchSubnet(ctx context.Context, network int64, ipRange, networkZone string, vswitch int64) (Action, error) {
	var body struct {
		Action Action `json:"action"`
	}
	err := c.do(ctx, http.MethodPost, "/networks/"+strconv.FormatInt(network, 10)+"/actions/add_subnet", map[string]any{
		"type": "vswitch", "ip_range": ipRange, "network_zone": networkZone, "vswitch_id": vswitch,
	}, &body)
	return body.Action, err
}
