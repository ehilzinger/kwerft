// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics reads time series from VictoriaMetrics for the console: a
// small client for /api/v1/query and /api/v1/query_range that confines every
// query to the namespaces a user may see, and the catalog of queries behind
// the console's charts (catalog.go), so the UI never sends PromQL except in
// the explorer.
//
// Confinement is VictoriaMetrics' own: the `extra_filters[]` query argument
// is joined into every series selector of the query, so a selector that
// names another namespace (or none, as {__name__=~".+"}) still only matches
// series of the allowed ones. The filter goes into the URL, never into the
// form body with the user's query: VictoriaMetrics reads extra_filters from
// the URL in preference to the body exactly so that a proxy's filter cannot
// be overridden.
package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Errors the console turns into answers.
var (
	// ErrUnavailable: VictoriaMetrics did not answer (not installed, down,
	// overloaded or too slow).
	ErrUnavailable = errors.New("metrics are unavailable")
)

// QueryError is VictoriaMetrics refusing a query: bad syntax, too many
// series, a range it will not serve. The message is VictoriaMetrics' own.
type QueryError struct{ Message string }

func (e *QueryError) Error() string { return e.Message }

// Scope is what a query may see. The zero value sees nothing, so a
// forgotten scope fails closed.
type Scope struct {
	all        bool
	namespaces []string
}

// Unconfined is for owners and admins: every namespace, the platform and
// the nodes.
func Unconfined() Scope { return Scope{all: true} }

// Namespaces confines queries to series whose namespace label is one of ns.
// Platform namespaces are dropped even if listed (a project can never be
// one, but the list comes from the cluster).
func Namespaces(ns ...string) Scope {
	var out []string
	for _, n := range ns {
		if dnsLabel.MatchString(n) && !IsPlatformNamespace(n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return Scope{namespaces: out}
}

// All reports whether the scope is unconfined.
func (s Scope) All() bool { return s.all }

// Empty reports whether the scope sees no series at all.
func (s Scope) Empty() bool { return !s.all && len(s.namespaces) == 0 }

// Filter is the series selector added to every query through
// extra_filters[]; empty when unconfined.
func (s Scope) Filter() string {
	if s.all {
		return ""
	}
	quoted := make([]string, len(s.namespaces))
	for i, n := range s.namespaces {
		quoted[i] = regexp.QuoteMeta(n) // DNS labels: a no-op, kept for safety
	}
	// VictoriaMetrics anchors regular expressions, like Prometheus.
	return `{namespace=~"` + strings.Join(quoted, "|") + `"}`
}

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// PlatformNamespaces run Kwerft and the cluster itself. Their series are for
// owners and admins only.
var PlatformNamespaces = []string{
	"kube-system", "kube-public", "kube-node-lease", "default",
	"kwerft-system", "kwerft-observability", "kwerft-builds",
	"traefik", "cert-manager",
}

// IsPlatformNamespace reports whether ns belongs to the platform, including
// any kube-* or kwerft-* namespace.
func IsPlatformNamespace(ns string) bool {
	return slices.Contains(PlatformNamespaces, ns) || strings.HasPrefix(ns, "kube-") || strings.HasPrefix(ns, "kwerft-")
}

// Point is one sample: unix seconds and value.
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series is one time series of a result. Instant queries have one point.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// Range is a query_range window.
type Range struct {
	Start, End time.Time
	Step       time.Duration
}

// Client queries one VictoriaMetrics (single-node) HTTP API.
type Client struct {
	// URL is the base, e.g. observability.MetricsURL.
	URL string
	// HTTP is the client; nil means one with Timeout.
	HTTP *http.Client
	// Timeout bounds every request; 0 means DefaultTimeout. VictoriaMetrics
	// gets it too, so it stops working on an abandoned query.
	Timeout time.Duration
}

// DefaultTimeout bounds a query.
const DefaultTimeout = 15 * time.Second

// maxBody caps a response (the explorer can ask for a lot).
const maxBody = 32 << 20

// New returns a client for base.
func New(base string) *Client { return &Client{URL: strings.TrimRight(base, "/")} }

// Query runs an instant query at t.
func (c *Client) Query(ctx context.Context, query string, t time.Time, scope Scope) ([]Series, error) {
	if scope.Empty() {
		return []Series{}, nil
	}
	form := url.Values{"query": {query}, "time": {unix(t)}}
	return c.do(ctx, "/api/v1/query", form, scope)
}

// QueryRange runs a range query.
func (c *Client) QueryRange(ctx context.Context, query string, r Range, scope Scope) ([]Series, error) {
	if scope.Empty() {
		return []Series{}, nil
	}
	if r.Step <= 0 || !r.End.After(r.Start) {
		return nil, &QueryError{Message: "invalid range"}
	}
	form := url.Values{
		"query": {query},
		"start": {unix(r.Start)},
		"end":   {unix(r.End)},
		"step":  {strconv.FormatInt(int64(r.Step/time.Second), 10) + "s"},
	}
	return c.do(ctx, "/api/v1/query_range", form, scope)
}

func unix(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func (c *Client) timeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultTimeout
}

func (c *Client) do(ctx context.Context, path string, form url.Values, scope Scope) ([]Series, error) {
	timeout := c.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	form.Set("timeout", strconv.Itoa(int(timeout/time.Second))+"s")

	u, err := url.Parse(strings.TrimRight(c.URL, "/") + path)
	if err != nil {
		return nil, fmt.Errorf("%w: bad URL: %v", ErrUnavailable, err)
	}
	// The confinement lives in the URL; see the package comment.
	q := url.Values{}
	if f := scope.Filter(); f != "" {
		q.Set("extra_filters[]", f)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: timeout + 5*time.Second}
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if len(body) > maxBody {
		return nil, &QueryError{Message: "The result is too large. Narrow the query or the time range."}
	}
	var out apiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		if res.StatusCode >= 500 || res.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, res.StatusCode)
		}
		return nil, fmt.Errorf("%w: unexpected answer (HTTP %d)", ErrUnavailable, res.StatusCode)
	}
	if out.Status != "success" {
		tooLong := strings.Contains(out.Error, "timeout") || strings.Contains(out.Error, "deadline")
		switch {
		case res.StatusCode == http.StatusServiceUnavailable && tooLong:
			return nil, &QueryError{Message: "The query took too long. Narrow it or pick a shorter time range."}
		case res.StatusCode >= 500, res.StatusCode == http.StatusTooManyRequests:
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrUnavailable, res.StatusCode, out.Error)
		}
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("query failed (HTTP %d)", res.StatusCode)
		}
		return nil, &QueryError{Message: msg}
	}
	return out.Data.series()
}

type apiResponse struct {
	Status string  `json:"status"`
	Error  string  `json:"error"`
	Data   apiData `json:"data"`
}

type apiData struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
}

type apiSeries struct {
	Metric map[string]string `json:"metric"`
	Values []sample          `json:"values"`
	Value  *sample           `json:"value"`
}

// sample is [unix seconds (float), "value"].
type sample [2]json.RawMessage

func (s sample) point() (Point, bool) {
	var t float64
	var v string
	if json.Unmarshal(s[0], &t) != nil || json.Unmarshal(s[1], &v) != nil {
		return Point{}, false
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return Point{}, false // JSON has no NaN; a gap in the chart
	}
	return Point{T: int64(t), V: f}, true
}

func (d apiData) series() ([]Series, error) {
	out := []Series{}
	switch d.ResultType {
	case "matrix", "vector":
		var raw []apiSeries
		if err := json.Unmarshal(d.Result, &raw); err != nil {
			return nil, fmt.Errorf("%w: cannot decode result: %v", ErrUnavailable, err)
		}
		for _, r := range raw {
			s := Series{Labels: r.Metric, Points: []Point{}}
			if s.Labels == nil {
				s.Labels = map[string]string{}
			}
			if r.Value != nil {
				if p, ok := r.Value.point(); ok {
					s.Points = append(s.Points, p)
				}
			}
			for _, v := range r.Values {
				if p, ok := v.point(); ok {
					s.Points = append(s.Points, p)
				}
			}
			out = append(out, s)
		}
	case "scalar":
		var v sample
		if err := json.Unmarshal(d.Result, &v); err != nil {
			return nil, fmt.Errorf("%w: cannot decode result: %v", ErrUnavailable, err)
		}
		s := Series{Labels: map[string]string{}, Points: []Point{}}
		if p, ok := v.point(); ok {
			s.Points = append(s.Points, p)
		}
		out = append(out, s)
	case "string", "":
		// Nothing to chart.
	default:
		return nil, fmt.Errorf("%w: unknown result type %q", ErrUnavailable, d.ResultType)
	}
	return out, nil
}
