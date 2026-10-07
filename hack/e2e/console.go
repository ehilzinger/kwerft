// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

// A client for the console's HTTP API, as a user would use it: HTTPS to the
// public hostname, a session cookie from signing in.

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// certChecker accepts a certificate a public CA issued for the hostname, or
// one from Let's Encrypt staging (the e2e runs use staging; its root is not
// in any trust store). Traefik's self-signed fallback is refused, so "HTTPS
// works" means a real ACME certificate was issued.
type certChecker struct {
	roots *x509.CertPool // nil: the system roots
	now   func() time.Time

	mu     sync.Mutex
	issuer map[string]string // hostname → issuer of the last certificate seen
}

func (c *certChecker) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		inter.AddCert(ic)
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: cs.ServerName, Roots: c.roots, Intermediates: inter, CurrentTime: now})
	if err != nil {
		if !isStaging(leaf) {
			return fmt.Errorf("certificate for %s: %w (issuer %q)", cs.ServerName, err, leaf.Issuer.String())
		}
		if herr := leaf.VerifyHostname(cs.ServerName); herr != nil {
			return herr
		}
		if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
			return fmt.Errorf("staging certificate for %s is not valid now", cs.ServerName)
		}
	}
	c.mu.Lock()
	if c.issuer == nil {
		c.issuer = map[string]string{}
	}
	c.issuer[cs.ServerName] = issuerName(leaf)
	c.mu.Unlock()
	return nil
}

func (c *certChecker) issuerOf(host string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issuer[host]
}

func isStaging(cert *x509.Certificate) bool {
	for _, s := range append([]string{cert.Issuer.CommonName}, cert.Issuer.Organization...) {
		if strings.Contains(s, "(STAGING)") {
			return true
		}
	}
	return false
}

func issuerName(cert *x509.Certificate) string {
	org := strings.Join(cert.Issuer.Organization, ", ")
	if org == "" {
		return cert.Issuer.CommonName
	}
	return org + " / " + cert.Issuer.CommonName
}

// newHTTPClient makes requests over TLS checked by certs; the jar keeps
// the session cookie.
func newHTTPClient(certs *certChecker) *http.Client {
	jar, _ := cookiejar.New(nil)
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		// The standard verification would refuse staging certificates;
		// certChecker.verify does it, with that one exception.
		InsecureSkipVerify: true, //nolint:gosec // verified in VerifyConnection
		VerifyConnection:   certs.verify,
		MinVersion:         tls.VersionTLS12,
	}
	return &http.Client{Transport: tr, Jar: jar, Timeout: 30 * time.Second}
}

type console struct {
	base string // https://<host>
	http *http.Client
}

// httpError is a non-2xx answer.
type httpError struct {
	Status int
	Body   string // shortened, for messages
	Raw    string // the whole answer
}

var errorFieldRE = regexp.MustCompile(`"error"\s*:\s*"((?:[^"\\]|\\.)*)"`)

// message is the answer's "error" field, else its shortened body.
func (e *httpError) message() string {
	var v struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(e.Raw), &v) == nil && v.Error != "" {
		return v.Error
	}
	if m := errorFieldRE.FindStringSubmatch(e.Body); m != nil {
		return m[1]
	}
	return strings.TrimSpace(e.Body)
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, strings.TrimSpace(e.Body))
}

func (c *console) do(ctx context.Context, method, path string, in, out any) error {
	var rd io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &httpError{Status: resp.StatusCode, Body: truncate(string(data), 300), Raw: string(data)}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func (c *console) healthz(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/healthz", nil, nil)
}

func (c *console) version(ctx context.Context) (string, error) {
	var v struct {
		Version string `json:"version"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/version", nil, &v)
	return v.Version, err
}

func (c *console) setupComplete(ctx context.Context) (bool, error) {
	var v struct {
		Complete bool `json:"complete"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/setup", nil, &v)
	return v.Complete, err
}

// createOwner is the setup wizard: the installer's one-time token, then the
// owner account (which also signs in).
func (c *console) createOwner(ctx context.Context, token string, o owner) error {
	if err := c.do(ctx, http.MethodPost, "/api/v1/setup/verify", map[string]string{"token": token}, nil); err != nil {
		return fmt.Errorf("setup token: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/api/v1/setup/owner", map[string]string{"name": o.Name, "email": o.Email, "password": o.Password}, nil)
}

func (c *console) signIn(ctx context.Context, o owner) error {
	if err := c.do(ctx, http.MethodPost, "/api/v1/session", map[string]string{"email": o.Email, "password": o.Password}, nil); err != nil {
		return err
	}
	var s struct {
		Email     string `json:"email"`
		Role      string `json:"role"`
		MustEnrol bool   `json:"mustEnrol"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/session", nil, &s); err != nil {
		return err
	}
	if !strings.EqualFold(s.Email, o.Email) || s.Role != "owner" {
		return fmt.Errorf("signed in as %q (%s), want %q (owner)", s.Email, s.Role, o.Email)
	}
	return nil
}

func (c *console) createProject(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/projects", map[string]string{"name": name, "displayName": "e2e " + name}, nil)
}

func (c *console) createApp(ctx context.Context, project, name string, spec kwerftv1.AppSpec) error {
	in := struct {
		Name string           `json:"name"`
		Spec kwerftv1.AppSpec `json:"spec"`
	}{name, spec}
	return c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/apps", in, nil)
}

// appSummary is the list view of an App (GET /api/v1/apps).
type appSummary struct {
	Name    string   `json:"name"`
	Project string   `json:"project"`
	Phase   string   `json:"phase"`
	Reason  string   `json:"reason"`
	Message string   `json:"message"`
	URLs    []string `json:"urls"`
}

func (c *console) app(ctx context.Context, project, name string) (*appSummary, error) {
	var list []appSummary
	if err := c.do(ctx, http.MethodGet, "/api/v1/apps?project="+url.QueryEscape(project), nil, &list); err != nil {
		return nil, err
	}
	for i := range list {
		if list[i].Name == name && list[i].Project == project {
			return &list[i], nil
		}
	}
	return nil, fmt.Errorf("app %s/%s not listed", project, name)
}

type pod struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
}

// appPods lists an App's replicas (the console answers {"pods": [...],
// "metrics": …, "access": …}).
func (c *console) appPods(ctx context.Context, project, name string) ([]pod, error) {
	var out struct {
		Pods []pod `json:"pods"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(name)+"/pods", nil, &out)
	return out.Pods, err
}

type build struct {
	Name          string `json:"name"`
	Phase         string `json:"phase"`
	StatusMessage string `json:"statusMessage"`
	Image         string `json:"image"`
}

func (c *console) buildNow(ctx context.Context, project, app string) (*build, error) {
	var b build
	err := c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/apps/"+url.PathEscape(app)+"/builds", map[string]string{}, &b)
	return &b, err
}

func (c *console) build(ctx context.Context, project, name string) (*build, error) {
	var b build
	err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/builds/"+url.PathEscape(name), nil, &b)
	return &b, err
}

func (c *console) createTask(ctx context.Context, project string, spec kwerftv1.TaskSpec) (string, error) {
	in := struct {
		Spec kwerftv1.TaskSpec `json:"spec"`
	}{spec}
	var t kwerftv1.Task
	if err := c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/tasks", in, &t); err != nil {
		return "", err
	}
	return t.Name, nil
}

func (c *console) task(ctx context.Context, project, name string) (*kwerftv1.Task, error) {
	var t kwerftv1.Task
	err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/tasks/"+url.PathEscape(name), nil, &t)
	return &t, err
}

// metricSeries runs an explorer query over the last 15 minutes and counts
// the series that have at least one point.
func (c *console) metricSeries(ctx context.Context, query string) (int, error) {
	var out struct {
		Series []struct {
			Points []json.RawMessage `json:"points"`
		} `json:"series"`
	}
	q := url.Values{"query": {query}, "range": {"15m"}}
	if err := c.do(ctx, http.MethodGet, "/api/v1/metrics/query?"+q.Encode(), nil, &out); err != nil {
		return 0, err
	}
	n := 0
	for _, s := range out.Series {
		if len(s.Points) > 0 {
			n++
		}
	}
	return n, nil
}

type logEntry struct {
	Project string `json:"project"`
	Task    string `json:"task"`
	App     string `json:"app"`
	Line    string `json:"line"`
}

func (c *console) searchLogs(ctx context.Context, project, query string) ([]logEntry, error) {
	var out struct {
		Entries []logEntry `json:"entries"`
	}
	q := url.Values{"project": {project}, "query": {query}, "since": {"1h"}}
	err := c.do(ctx, http.MethodGet, "/api/v1/logs?"+q.Encode(), nil, &out)
	return out.Entries, err
}

type alert struct {
	Rule     string    `json:"rule"`
	State    string    `json:"state"`
	Project  string    `json:"project"`
	App      string    `json:"app"`
	Summary  string    `json:"summary"`
	StartsAt time.Time `json:"startsAt"`
}

func (c *console) alerts(ctx context.Context) ([]alert, error) {
	var out []alert
	err := c.do(ctx, http.MethodGet, "/api/v1/alerts", nil, &out)
	return out, err
}

// get fetches an arbitrary HTTPS URL (an app's public hostname) and returns
// status and body.
func (c *console) get(ctx context.Context, u string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, string(data), err
}
