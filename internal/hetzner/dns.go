// Package hetzner is a small client for Hetzner DNS, which lives in the
// Hetzner Cloud API since the old DNS Console (dns.hetzner.com) was shut down
// in May 2026: zones and RRsets at api.hetzner.cloud/v1, authorised with a
// Cloud project API token. Only what Kwerft needs: find a host's zone, and
// keep A/AAAA RRsets it owns (by label) pointing at the cluster.
package hetzner

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
	"strings"
	"time"
)

// CloudAPI is the production endpoint.
const CloudAPI = "https://api.hetzner.cloud/v1"

var (
	// ErrTokenRejected: the API answered 401 or 403.
	ErrTokenRejected = errors.New("token rejected")
	// ErrNoZone: no zone in the token's project contains the host.
	ErrNoZone = errors.New("no zone")
	// ErrNotFound: the zone or RRset does not exist.
	ErrNotFound = errors.New("not found")
)

// RateLimitError is a 429; Reset is when requests are possible again.
type RateLimitError struct{ Reset time.Time }

func (e *RateLimitError) Error() string {
	return "Hetzner API rate limit reached until " + e.Reset.UTC().Format(time.RFC3339)
}

// APIError is any other error answer.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Hetzner API: %s (%s)", e.Message, e.Code)
	}
	return fmt.Sprintf("Hetzner API answered %d %s", e.Status, http.StatusText(e.Status))
}

// Client talks to the Cloud API with one project token.
type Client struct {
	Token string
	// Base defaults to CloudAPI.
	Base string
	// HTTP defaults to a client with a 15 s timeout.
	HTTP *http.Client
}

var defaultHTTP = &http.Client{Timeout: 15 * time.Second}

// Zone is a DNS zone. Only primary zones have RRsets Kwerft can manage.
type Zone struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"` // primary | secondary
}

// Record is one value of an RRset.
type Record struct {
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// RRSet is all records of one name and type in a zone. Name is relative to
// the zone: "@" for the apex, "*.apps" for a wildcard.
type RRSet struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"`
	TTL     *int              `json:"ttl,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Records []Record          `json:"records"`
}

// Values are the record values in the order the API returned them.
func (s RRSet) Values() []string {
	out := make([]string, 0, len(s.Records))
	for _, r := range s.Records {
		out = append(out, r.Value)
	}
	return out
}

// Zones lists every zone of the token's project.
func (c *Client) Zones(ctx context.Context) ([]Zone, error) {
	var out []Zone
	for page := 1; ; page++ {
		var body struct {
			Zones []Zone `json:"zones"`
			Meta  meta   `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"50"}}
		if err := c.do(ctx, http.MethodGet, "/zones?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.Zones...)
		if body.Meta.Pagination.NextPage == nil || len(body.Zones) == 0 {
			return out, nil
		}
	}
}

// ZoneFor finds the zone in the token's project that contains host, trying
// host itself and then each parent with at least two labels.
func (c *Client) ZoneFor(ctx context.Context, host string) (string, error) {
	labels := strings.Split(host, ".")
	for i := 0; i <= len(labels)-2; i++ {
		zone := strings.Join(labels[i:], ".")
		var body struct {
			Zones []Zone `json:"zones"`
		}
		if err := c.do(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(zone), nil, &body); err != nil {
			return "", err
		}
		for _, z := range body.Zones {
			if strings.EqualFold(strings.TrimSuffix(z.Name, "."), zone) {
				return zone, nil
			}
		}
	}
	return "", ErrNoZone
}

// MatchZone returns the zone of zones that contains host (the longest
// suffix), and host's name relative to it ("@" for the apex).
func MatchZone(zones []Zone, host string) (Zone, string, bool) {
	var best Zone
	found := false
	for _, z := range zones {
		name := strings.ToLower(strings.TrimSuffix(z.Name, "."))
		if (host == name || strings.HasSuffix(host, "."+name)) && len(name) > len(best.Name) {
			best, found = Zone{ID: z.ID, Name: name, Mode: z.Mode}, true
		}
	}
	if !found {
		return Zone{}, "", false
	}
	if host == best.Name {
		return best, "@", true
	}
	return best, strings.TrimSuffix(host, "."+best.Name), true
}

// RRSets lists the zone's RRsets of the given types (all types when none).
func (c *Client) RRSets(ctx context.Context, zone string, types ...string) ([]RRSet, error) {
	var out []RRSet
	for page := 1; ; page++ {
		var body struct {
			RRSets []RRSet `json:"rrsets"`
			Meta   meta    `json:"meta"`
		}
		q := url.Values{"page": {strconv.Itoa(page)}, "per_page": {"100"}}
		for _, t := range types {
			q.Add("type", t)
		}
		if err := c.do(ctx, http.MethodGet, "/zones/"+url.PathEscape(zone)+"/rrsets?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		out = append(out, body.RRSets...)
		if body.Meta.Pagination.NextPage == nil || len(body.RRSets) == 0 {
			return out, nil
		}
	}
}

// CreateRRSet creates an RRset; it fails when one of that name and type exists.
func (c *Client) CreateRRSet(ctx context.Context, zone string, set RRSet) error {
	return c.do(ctx, http.MethodPost, "/zones/"+url.PathEscape(zone)+"/rrsets", set, nil)
}

// SetRecords replaces the records of an existing RRset.
func (c *Client) SetRecords(ctx context.Context, zone, name, typ string, records []Record) error {
	return c.do(ctx, http.MethodPost, rrsetPath(zone, name, typ)+"/actions/set_records", map[string]any{"records": records}, nil)
}

// SetLabels replaces the labels of an RRset.
func (c *Client) SetLabels(ctx context.Context, zone, name, typ string, labels map[string]string) error {
	return c.do(ctx, http.MethodPut, rrsetPath(zone, name, typ), map[string]any{"labels": labels}, nil)
}

// DeleteRRSet deletes an RRset; a missing one is not an error.
func (c *Client) DeleteRRSet(ctx context.Context, zone, name, typ string) error {
	err := c.do(ctx, http.MethodDelete, rrsetPath(zone, name, typ), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func rrsetPath(zone, name, typ string) string {
	return "/zones/" + url.PathEscape(zone) + "/rrsets/" + url.PathEscape(name) + "/" + url.PathEscape(typ)
}

type meta struct {
	Pagination struct {
		NextPage *int `json:"next_page"`
	} `json:"pagination"`
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	base := c.Base
	if base == "" {
		base = CloudAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
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
	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return ErrTokenRejected
	case res.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case res.StatusCode == http.StatusTooManyRequests:
		reset := time.Now().Add(time.Minute)
		if s, err := strconv.ParseInt(res.Header.Get("RateLimit-Reset"), 10, 64); err == nil {
			reset = time.Unix(s, 0)
		}
		return &RateLimitError{Reset: reset}
	case res.StatusCode >= 300:
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.NewDecoder(limited).Decode(&e)
		return &APIError{Status: res.StatusCode, Code: e.Error.Code, Message: e.Error.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(out); err != nil {
		return fmt.Errorf("Hetzner API: %w", err)
	}
	return nil
}
