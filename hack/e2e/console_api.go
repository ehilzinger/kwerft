package main

// The parts of the console's API the upgrade, k3s and restore runs use:
// upgrades and their event stream, nodes and join commands, backups,
// volumes and secret sets.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// withTimeout is the console with another request timeout: the upgrade
// preflight takes up to 45 s, an event stream an hour (0: no timeout).
func (c *console) withTimeout(d time.Duration) *console {
	hc := *c.http
	hc.Timeout = d
	return &console{base: c.base, http: &hc}
}

// ---- upgrades (internal/server/api_upgrades.go) ------------------------------------

type upgradeCheck struct {
	Check   string `json:"check"`
	OK      bool   `json:"ok"`
	Warning bool   `json:"warning,omitempty"`
	Message string `json:"message,omitempty"`
}

type upgradeStep struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	State  string `json:"state"` // Running, Done, Skipped, Failed
	Detail string `json:"detail,omitempty"`
}

type upgradeNode struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	State   string `json:"state"` // Waiting, Draining, Upgrading, Done, Failed
	Message string `json:"message,omitempty"`
}

type upgradeFrom struct {
	Kwerft     string `json:"kwerft,omitempty"`
	Kubernetes string `json:"kubernetes,omitempty"`
}

// upgradeView is an Upgrade as the console shows it.
type upgradeView struct {
	Name      string         `json:"name"`
	Cluster   string         `json:"cluster"`
	Component string         `json:"component"`
	Version   string         `json:"version"`
	Phase     string         `json:"phase"`
	From      *upgradeFrom   `json:"from,omitempty"`
	Preflight []upgradeCheck `json:"preflight"`
	Steps     []upgradeStep  `json:"steps"`
	Nodes     []upgradeNode  `json:"nodes"`
	Reason    string         `json:"reason,omitempty"`
	Message   string         `json:"message,omitempty"`
	Finished  bool           `json:"finished"`
}

type upgradeRequest struct {
	Cluster        string `json:"cluster"`
	Component      string `json:"component"` // Kwerft | Kubernetes
	Version        string `json:"version"`
	Password       string `json:"password"`
	ConfirmVersion string `json:"confirmVersion,omitempty"`
}

// startUpgrade is Settings › Updates › Upgrade…: the synchronous preflight,
// then the Upgrade, created as the owner.
func (c *console) startUpgrade(ctx context.Context, req upgradeRequest) (*upgradeView, error) {
	var out struct {
		Upgrade upgradeView `json:"upgrade"`
	}
	if err := c.withTimeout(2*time.Minute).do(ctx, http.MethodPost, "/api/v1/upgrades", req, &out); err != nil {
		return nil, err
	}
	return &out.Upgrade, nil
}

func (c *console) upgrade(ctx context.Context, name string) (*upgradeView, error) {
	var u upgradeView
	err := c.do(ctx, http.MethodGet, "/api/v1/upgrades/"+url.PathEscape(name), nil, &u)
	return &u, err
}

func (c *console) upgradeLog(ctx context.Context, name string) (string, error) {
	var out struct {
		Log string `json:"log"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/upgrades/"+url.PathEscape(name)+"/log", nil, &out)
	return out.Log, err
}

// streamEnd is how an event stream ended: "end" with the phase (or the
// reason "session"), "gone", or neither when the connection broke.
type streamEnd struct {
	Phase  string
	Reason string
	Gone   bool
}

// upgradeEvents reads GET /api/v1/upgrades/{name}/events (Server-Sent
// Events) until it ends, handing each "upgrade" event to fn.
func (c *console) upgradeEvents(ctx context.Context, name string, fn func(upgradeView)) (streamEnd, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/upgrades/"+url.PathEscape(name)+"/events", nil)
	if err != nil {
		return streamEnd{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.withTimeout(0).http.Do(req)
	if err != nil {
		return streamEnd{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return streamEnd{}, &httpError{Status: resp.StatusCode, Body: truncate(string(data), 300)}
	}
	return readEvents(resp.Body, fn)
}

// readEvents parses an event stream: "event:" and "data:" lines, a blank
// line ends an event, comments (":") keep the connection open. A stream
// that stops without "end" or "gone" is io.ErrUnexpectedEOF.
func readEvents(r io.Reader, fn func(upgradeView)) (streamEnd, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var event string
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "":
			payload, name := data.String(), event
			event = ""
			data.Reset()
			switch name {
			case "upgrade":
				var u upgradeView
				if err := json.Unmarshal([]byte(payload), &u); err != nil {
					return streamEnd{}, fmt.Errorf("event upgrade: %w", err)
				}
				fn(u)
			case "end":
				var e struct {
					Phase  string `json:"phase"`
					Reason string `json:"reason"`
				}
				_ = json.Unmarshal([]byte(payload), &e)
				return streamEnd{Phase: e.Phase, Reason: e.Reason}, nil
			case "gone":
				return streamEnd{Gone: true}, nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return streamEnd{}, err
	}
	return streamEnd{}, io.ErrUnexpectedEOF
}

// ---- nodes (internal/server/api_nodes.go) ---------------------------------------------

type nodeView struct {
	Name          string   `json:"name"`
	Roles         []string `json:"roles"`
	Ready         bool     `json:"ready"`
	Status        string   `json:"status"`
	Kubelet       string   `json:"kubeletVersion,omitempty"`
	Unschedulable bool     `json:"unschedulable"`
}

func (c *console) nodes(ctx context.Context, cluster string) ([]nodeView, error) {
	var out struct {
		Reachable bool       `json:"reachable"`
		Problem   string     `json:"problem"`
		Nodes     []nodeView `json:"nodes"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/clusters/"+url.PathEscape(cluster)+"/nodes", nil, &out); err != nil {
		return nil, err
	}
	if !out.Reachable {
		return nil, fmt.Errorf("cluster %s is not reachable: %s", cluster, out.Problem)
	}
	return out.Nodes, nil
}

var joinTokenRE = regexp.MustCompile(`--token\s+(\S+)`)

// joinToken asks for a join command (Nodes › Add node) and returns its
// token.
func (c *console) joinToken(ctx context.Context, cluster, role string) (string, error) {
	var out struct {
		Command string `json:"command"`
	}
	in := map[string]any{"role": role, "ttlMinutes": 60}
	if err := c.do(ctx, http.MethodPost, "/api/v1/clusters/"+url.PathEscape(cluster)+"/join-command", in, &out); err != nil {
		return "", err
	}
	m := joinTokenRE.FindStringSubmatch(out.Command)
	if m == nil {
		return "", errors.New("the join command has no --token")
	}
	return m[1], nil
}

// ---- backups (internal/server/api_backups.go) ---------------------------------------

type backupTarget struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region,omitempty"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

type backupSettings struct {
	Configured bool   `json:"configured"`
	Prefix     string `json:"prefix"`
	State      string `json:"state"` // NotConfigured, Pending, Ready, Error
	Message    string `json:"message"`
}

// saveBackupTarget is Settings › Backups › Save; the first save answers
// the recovery key, once.
func (c *console) saveBackupTarget(ctx context.Context, t backupTarget) (recoveryKey string, planCreated bool, err error) {
	var out struct {
		RecoveryKey string `json:"recoveryKey"`
		PlanCreated bool   `json:"planCreated"`
	}
	err = c.withTimeout(2*time.Minute).do(ctx, http.MethodPut, "/api/v1/settings/backups", t, &out)
	return out.RecoveryKey, out.PlanCreated, err
}

func (c *console) backupSettings(ctx context.Context) (*backupSettings, error) {
	var out backupSettings
	err := c.do(ctx, http.MethodGet, "/api/v1/settings/backups", nil, &out)
	return &out, err
}

// runBackupPlan is "Back up now"; it answers the Backup's name.
func (c *console) runBackupPlan(ctx context.Context, plan string) (string, error) {
	var out struct {
		Backup string `json:"backup"`
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/backups/plans/"+url.PathEscape(plan)+"/run", map[string]any{}, &out)
	return out.Backup, err
}

type backupView struct {
	Name     string `json:"name"`
	Plan     string `json:"plan"`
	Scope    string `json:"scope"`
	Phase    string `json:"phase"`
	Items    int64  `json:"items"`
	Warnings int64  `json:"warnings"`
	Errors   int64  `json:"errors"`
	Bytes    int64  `json:"bytes"`
	Message  string `json:"message"`
}

func (c *console) backups(ctx context.Context) ([]backupView, error) {
	var out struct {
		Velero  bool         `json:"velero"`
		Backups []backupView `json:"backups"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/backups", nil, &out); err != nil {
		return nil, err
	}
	if !out.Velero {
		return nil, errors.New("velero is not installed")
	}
	return out.Backups, nil
}

// ---- volumes and secret sets -----------------------------------------------------

func (c *console) createVolume(ctx context.Context, project, name, size string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/volumes", map[string]string{"name": name, "size": size}, nil)
}

func (c *console) createSecretSet(ctx context.Context, project, name string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/projects/"+url.PathEscape(project)+"/secret-sets", map[string]any{"name": name, "description": "e2e restore run"}, nil)
}

// setSecret writes a value; no answer ever carries it.
func (c *console) setSecret(ctx context.Context, project, set, key, value string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/projects/"+url.PathEscape(project)+"/secret-sets/"+url.PathEscape(set)+"/keys/"+url.PathEscape(key), map[string]string{"value": value}, nil)
}

// secretKeys lists the key names of a secret set.
func (c *console) secretKeys(ctx context.Context, project, set string) ([]string, error) {
	var out struct {
		Keys []struct {
			Name string `json:"name"`
		} `json:"keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/projects/"+url.PathEscape(project)+"/secret-sets/"+url.PathEscape(set), nil, &out); err != nil {
		return nil, err
	}
	var keys []string
	for _, k := range out.Keys {
		keys = append(keys, k.Name)
	}
	return keys, nil
}
