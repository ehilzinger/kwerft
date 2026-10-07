// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package alerting

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/ehilzinger/kwerft/internal/observability"
)

// ErrUnavailable wraps errors reaching Alertmanager or VictoriaMetrics.
var ErrUnavailable = errors.New("unavailable")

// ErrNotFound: Alertmanager does not know the silence.
var ErrNotFound = errors.New("not found")

func defaultHTTP(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// ---- Alertmanager (API v2) ---------------------------------------------------------

// Alertmanager talks to Alertmanager's API v2. Alertmanager has no
// authentication of its own: only the console reaches it (in-cluster), and
// the console decides who may see and silence what.
type Alertmanager struct {
	URL  string // empty: observability.AlertmanagerURL
	HTTP *http.Client
}

// AMAlert is an alert as Alertmanager reports it.
type AMAlert struct {
	Fingerprint  string            `json:"fingerprint"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	UpdatedAt    time.Time         `json:"updatedAt"`
	GeneratorURL string            `json:"generatorURL"`
	Status       struct {
		State       string   `json:"state"` // active, suppressed, unprocessed
		SilencedBy  []string `json:"silencedBy"`
		InhibitedBy []string `json:"inhibitedBy"`
	} `json:"status"`
}

// Matcher is a silence matcher.
type Matcher struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	IsRegex bool   `json:"isRegex"`
	IsEqual *bool  `json:"isEqual,omitempty"`
}

// Equal reports whether the matcher is a plain equality (name="value").
func (m Matcher) Equal() bool { return !m.IsRegex && (m.IsEqual == nil || *m.IsEqual) }

// Silence is an Alertmanager silence.
type Silence struct {
	ID        string    `json:"id,omitempty"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"startsAt"`
	EndsAt    time.Time `json:"endsAt"`
	CreatedBy string    `json:"createdBy"`
	Comment   string    `json:"comment"`
	Status    *struct {
		State string `json:"state"` // active, pending, expired
	} `json:"status,omitempty"`
}

func (a *Alertmanager) base() string {
	if a.URL != "" {
		return a.URL
	}
	return observability.AlertmanagerURL
}

func (a *Alertmanager) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base()+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := defaultHTTP(a.HTTP).Do(req)
	if err != nil {
		return fmt.Errorf("%w: Alertmanager: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	switch {
	case res.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case res.StatusCode >= 500:
		return fmt.Errorf("%w: Alertmanager answered %d", ErrUnavailable, res.StatusCode)
	case res.StatusCode >= 300:
		return fmt.Errorf("alertmanager answered %d: %s", res.StatusCode, truncate(string(raw), 300))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("%w: Alertmanager: %v", ErrUnavailable, err)
		}
	}
	return nil
}

// Alerts returns Kwerft's alerts (those with kwerft_rule), firing and
// silenced; the stack's own default rules are left out.
func (a *Alertmanager) Alerts(ctx context.Context) ([]AMAlert, error) {
	q := url.Values{"filter": {observability.LabelRule + `=~".+"`}, "active": {"true"}, "silenced": {"true"}, "inhibited": {"true"}}
	var out []AMAlert
	return out, a.do(ctx, http.MethodGet, "/api/v2/alerts?"+q.Encode(), nil, &out)
}

// Silences returns every silence Alertmanager knows, expired ones included.
func (a *Alertmanager) Silences(ctx context.Context) ([]Silence, error) {
	var out []Silence
	return out, a.do(ctx, http.MethodGet, "/api/v2/silences", nil, &out)
}

// Silence returns one silence.
func (a *Alertmanager) Silence(ctx context.Context, id string) (*Silence, error) {
	var out Silence
	if err := a.do(ctx, http.MethodGet, "/api/v2/silence/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateSilence stores a silence and returns its ID.
func (a *Alertmanager) CreateSilence(ctx context.Context, s Silence) (string, error) {
	s.ID, s.Status = "", nil
	var out struct {
		SilenceID string `json:"silenceID"`
	}
	if err := a.do(ctx, http.MethodPost, "/api/v2/silences", s, &out); err != nil {
		return "", err
	}
	return out.SilenceID, nil
}

// DeleteSilence expires a silence.
func (a *Alertmanager) DeleteSilence(ctx context.Context, id string) error {
	return a.do(ctx, http.MethodDelete, "/api/v2/silence/"+url.PathEscape(id), nil, nil)
}

// ---- VictoriaMetrics (instant queries) ----------------------------------------------

// Metrics runs instant queries against VictoriaMetrics with the console's
// own identity; callers confine results themselves.
type Metrics struct {
	URL  string // empty: observability.MetricsURL
	HTTP *http.Client
}

// Sample is one series of an instant query result.
type Sample struct {
	Metric map[string]string
	Value  float64
}

// Query runs an instant query at t.
func (m *Metrics) Query(ctx context.Context, query string, t time.Time) ([]Sample, error) {
	base := m.URL
	if base == "" {
		base = observability.MetricsURL
	}
	q := url.Values{"query": {query}, "time": {strconv.FormatInt(t.Unix(), 10)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/query?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	res, err := defaultHTTP(m.HTTP).Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: VictoriaMetrics: %v", ErrUnavailable, err)
	}
	defer res.Body.Close()
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("%w: VictoriaMetrics answered %d", ErrUnavailable, res.StatusCode)
	}
	if out.Status != "success" {
		return nil, fmt.Errorf("victoriametrics: %s", out.Error)
	}
	samples := make([]Sample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		s, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			continue
		}
		samples = append(samples, Sample{Metric: r.Metric, Value: v})
	}
	return samples, nil
}

// ---- fingerprints -----------------------------------------------------------------------

// Fingerprint is Alertmanager's fingerprint of a label set (Prometheus'
// model.LabelSet.Fingerprint: FNV-1a over sorted names and values), so an
// alert resolved from the ALERTS series gets the ID it had while firing.
func Fingerprint(labels map[string]string) string {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
		sep      = 255
	)
	names := make([]string, 0, len(labels))
	for k := range labels {
		names = append(names, k)
	}
	slices.Sort(names)
	h := uint64(offset64)
	add := func(s string) {
		for i := 0; i < len(s); i++ {
			h ^= uint64(s[i])
			h *= prime64
		}
		h ^= sep
		h *= prime64
	}
	for _, n := range names {
		add(n)
		add(labels[n])
	}
	return fmt.Sprintf("%016x", h)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
