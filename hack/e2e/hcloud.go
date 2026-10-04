package main

// A minimal Hetzner Cloud API client: just what the e2e runs need (servers,
// SSH keys, server types, images). It lives here rather than in
// internal/hetzner so the harness stays independent of the product's client,
// which grows its own server and SSH key support for node pools.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const hcloudAPI = "https://api.hetzner.cloud/v1"

var errNotFound = errors.New("not found")

// apiError is an error answer of the Cloud API.
type apiError struct {
	Status  int
	Code    string
	Message string
}

func (e *apiError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Hetzner API %d: %s (%s)", e.Status, e.Message, e.Code)
	}
	return fmt.Sprintf("Hetzner API answered %d %s", e.Status, http.StatusText(e.Status))
}

// retryable: rate limits and server-side trouble; worth another try.
func (e *apiError) retryable() bool { return e.Status == http.StatusTooManyRequests || e.Status >= 500 }

type hcloud struct {
	token string
	base  string
	http  *http.Client
	// sleep waits between retries; tests make it instant.
	sleep func(context.Context, time.Duration) error
}

func newHcloud(token, base string) *hcloud {
	if base == "" {
		base = hcloudAPI
	}
	return &hcloud{token: token, base: base, http: &http.Client{Timeout: 30 * time.Second}, sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type hServer struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status"`
	Created   time.Time         `json:"created"`
	Labels    map[string]string `json:"labels"`
	PublicNet struct {
		IPv4 struct {
			IP string `json:"ip"`
		} `json:"ipv4"`
	} `json:"public_net"`
}

type hSSHKey struct {
	ID      int64             `json:"id"`
	Name    string            `json:"name"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels"`
}

type hImage struct {
	ID          int64      `json:"id"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Deprecated  *time.Time `json:"deprecated"`
}

type hServerType struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Cores       int     `json:"cores"`
	Memory      float64 `json:"memory"`
	Disk        int     `json:"disk"`
	Deprecation *struct {
		UnavailableAfter time.Time `json:"unavailable_after"`
	} `json:"deprecation"`
	Prices []hPrice `json:"prices"`
}

type hPrice struct {
	Location    string `json:"location"`
	PriceHourly struct {
		Gross string `json:"gross"`
	} `json:"price_hourly"`
}

// hourlyGross is the price per hour (EUR, incl. VAT) at location, "" when unknown.
func (t *hServerType) hourlyGross(location string) string {
	for _, p := range t.Prices {
		if p.Location == location {
			return p.PriceHourly.Gross
		}
	}
	return ""
}

type createServerRequest struct {
	Name             string            `json:"name"`
	ServerType       string            `json:"server_type"`
	Location         string            `json:"location"`
	Image            string            `json:"image"`
	SSHKeys          []int64           `json:"ssh_keys"`
	Labels           map[string]string `json:"labels"`
	UserData         string            `json:"user_data,omitempty"`
	StartAfterCreate bool              `json:"start_after_create"`
	PublicNet        struct {
		EnableIPv4 bool `json:"enable_ipv4"`
		EnableIPv6 bool `json:"enable_ipv6"`
	} `json:"public_net"`
}

// do sends one request; reads and deletes are retried on 429 and 5xx.
func (c *hcloud) do(ctx context.Context, method, path string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	attempts := 1
	if method == http.MethodGet || method == http.MethodDelete {
		attempts = 5
	}
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			if serr := c.sleep(ctx, time.Duration(1<<i)*time.Second); serr != nil {
				return err
			}
		}
		err = c.once(ctx, method, path, body, out)
		var ae *apiError
		if err == nil || !errors.As(err, &ae) || !ae.retryable() {
			return err
		}
	}
	return err
}

func (c *hcloud) once(ctx context.Context, method, path string, body []byte, out any) error {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{Status: resp.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

type pageMeta struct {
	Pagination struct {
		NextPage *int `json:"next_page"`
	} `json:"pagination"`
}

func (c *hcloud) serverType(ctx context.Context, name string) (*hServerType, error) {
	var body struct {
		ServerTypes []hServerType `json:"server_types"`
	}
	if err := c.do(ctx, http.MethodGet, "/server_types?"+url.Values{"name": {name}}.Encode(), nil, &body); err != nil {
		return nil, err
	}
	if len(body.ServerTypes) == 0 {
		return nil, errNotFound
	}
	return &body.ServerTypes[0], nil
}

func (c *hcloud) image(ctx context.Context, name string) (*hImage, error) {
	q := url.Values{"type": {"system"}, "name": {name}, "architecture": {"x86"}}
	var body struct {
		Images []hImage `json:"images"`
	}
	if err := c.do(ctx, http.MethodGet, "/images?"+q.Encode(), nil, &body); err != nil {
		return nil, err
	}
	for i := range body.Images {
		if body.Images[i].Deprecated == nil {
			return &body.Images[i], nil
		}
	}
	return nil, errNotFound
}

func (c *hcloud) createSSHKey(ctx context.Context, name, publicKey string, labels map[string]string) (*hSSHKey, error) {
	in := map[string]any{"name": name, "public_key": publicKey, "labels": labels}
	var body struct {
		SSHKey hSSHKey `json:"ssh_key"`
	}
	if err := c.do(ctx, http.MethodPost, "/ssh_keys", in, &body); err != nil {
		return nil, err
	}
	return &body.SSHKey, nil
}

// deleteSSHKey treats a key that is already gone as deleted.
func (c *hcloud) deleteSSHKey(ctx context.Context, id int64) error {
	if err := c.do(ctx, http.MethodDelete, "/ssh_keys/"+strconv.FormatInt(id, 10), nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

func (c *hcloud) createServer(ctx context.Context, req createServerRequest) (*hServer, error) {
	var body struct {
		Server hServer `json:"server"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", req, &body); err != nil {
		return nil, err
	}
	return &body.Server, nil
}

func (c *hcloud) server(ctx context.Context, id int64) (*hServer, error) {
	var body struct {
		Server hServer `json:"server"`
	}
	if err := c.do(ctx, http.MethodGet, "/servers/"+strconv.FormatInt(id, 10), nil, &body); err != nil {
		return nil, err
	}
	return &body.Server, nil
}

// deleteServer treats a server that is already gone as deleted.
func (c *hcloud) deleteServer(ctx context.Context, id int64) error {
	if err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(id, 10), nil, nil); err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	return nil
}

func (c *hcloud) servers(ctx context.Context, selector string) ([]hServer, error) {
	var out []hServer
	for page := 1; ; page++ {
		var body struct {
			Servers []hServer `json:"servers"`
			Meta    pageMeta  `json:"meta"`
		}
		q := url.Values{"label_selector": {selector}, "page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if err := c.do(ctx, http.MethodGet, "/servers?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Servers...)
		if body.Meta.Pagination.NextPage == nil || len(body.Servers) == 0 {
			return out, nil
		}
	}
}

func (c *hcloud) sshKeys(ctx context.Context, selector string) ([]hSSHKey, error) {
	var out []hSSHKey
	for page := 1; ; page++ {
		var body struct {
			SSHKeys []hSSHKey `json:"ssh_keys"`
			Meta    pageMeta  `json:"meta"`
		}
		q := url.Values{"label_selector": {selector}, "page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if err := c.do(ctx, http.MethodGet, "/ssh_keys?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.SSHKeys...)
		if body.Meta.Pagination.NextPage == nil || len(body.SSHKeys) == 0 {
			return out, nil
		}
	}
}
