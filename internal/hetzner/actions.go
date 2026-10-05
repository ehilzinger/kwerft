package hetzner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Action is an asynchronous Cloud API operation (creating a server,
// attaching a network, ...). Kwerft's reconcilers do not wait on actions:
// they look at the resource again on the next pass.
type Action struct {
	ID       int64  `json:"id"`
	Command  string `json:"command"`
	Status   string `json:"status"` // running | success | error
	Progress int    `json:"progress"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Action statuses.
const (
	ActionRunning = "running"
	ActionSuccess = "success"
	ActionError   = "error"
)

// Err is the action's failure, or nil while it runs or after it succeeded.
func (a Action) Err() error {
	if a.Status != ActionError {
		return nil
	}
	if a.Error == nil {
		return fmt.Errorf("Hetzner action %s failed", a.Command)
	}
	return &APIError{Status: http.StatusOK, Code: a.Error.Code, Message: a.Error.Message}
}

// Action reads one action by ID (GET /actions/{id}).
func (c *Client) Action(ctx context.Context, id int64) (Action, error) {
	var body struct {
		Action Action `json:"action"`
	}
	err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &body)
	return body.Action, err
}

// pagedList reads every page of a Cloud API list (path without query), the
// items under key: GET /servers answers {"servers": [...], "meta": {...}}.
func pagedList[T any](ctx context.Context, c *Client, path, key string, q url.Values) ([]T, error) {
	out := []T{}
	if q == nil {
		q = url.Values{}
	}
	for page := 1; ; page++ {
		q.Set("page", strconv.Itoa(page))
		q.Set("per_page", "50")
		var body map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &body); err != nil {
			return nil, err
		}
		var items []T
		if raw, ok := body[key]; ok {
			if err := json.Unmarshal(raw, &items); err != nil {
				return nil, fmt.Errorf("Hetzner API: %s: %w", key, err)
			}
		}
		out = append(out, items...)
		var m meta
		if raw, ok := body["meta"]; ok {
			_ = json.Unmarshal(raw, &m)
		}
		if m.Pagination.NextPage == nil || len(items) == 0 {
			return out, nil
		}
	}
}

// selector is a query with a label selector, or none when it is empty.
func selector(labelSelector string) url.Values {
	q := url.Values{}
	if labelSelector != "" {
		q.Set("label_selector", labelSelector)
	}
	return q
}
