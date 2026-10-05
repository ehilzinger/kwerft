package hetzner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Actions: most changing Cloud API calls answer with one or more Actions
// that finish later (status running → success | error). The helpers here
// wait for them, so callers see a change only once Hetzner has applied it.

// Action statuses.
const (
	ActionRunning = "running"
	ActionSuccess = "success"
	ActionFailed  = "error"
)

// Action is one asynchronous operation (GET /actions/{id}).
type Action struct {
	ID       int64        `json:"id"`
	Command  string       `json:"command"`
	Status   string       `json:"status"`
	Progress int          `json:"progress"`
	Error    *ActionError `json:"error,omitempty"`
}

// ActionError is why an action failed.
type ActionError struct {
	Command string `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ActionError) Error() string {
	return fmt.Sprintf("Hetzner action %s failed: %s (%s)", e.Command, e.Message, e.Code)
}

// ActionPoll is the first wait between polls of a running action; it doubles
// up to two seconds. Tests make it small.
var ActionPoll = 500 * time.Millisecond

// ActionTimeout bounds WaitActions when ctx has no earlier deadline.
var ActionTimeout = 3 * time.Minute

// Action reads one action.
func (c *Client) Action(ctx context.Context, id int64) (Action, error) {
	var body struct {
		Action Action `json:"action"`
	}
	err := c.do(ctx, http.MethodGet, "/actions/"+strconv.FormatInt(id, 10), nil, &body)
	return body.Action, err
}

// WaitActions polls the actions until each has finished. The first failed
// one is returned as an *ActionError.
func (c *Client) WaitActions(ctx context.Context, actions ...Action) error {
	ctx, cancel := context.WithTimeout(ctx, ActionTimeout)
	defer cancel()
	wait := ActionPoll
	for _, a := range actions {
		for a.Status == ActionRunning || a.Status == "" {
			select {
			case <-ctx.Done():
				return fmt.Errorf("waiting for Hetzner action %s (%d): %w", a.Command, a.ID, ctx.Err())
			case <-time.After(wait):
			}
			wait = min(wait*2, 2*time.Second)
			next, err := c.Action(ctx, a.ID)
			if err != nil {
				return err
			}
			a = next
		}
		if a.Status == ActionFailed {
			e := &ActionError{Command: a.Command, Code: "action_failed", Message: "the action failed"}
			if a.Error != nil {
				e.Code, e.Message = a.Error.Code, a.Error.Message
			}
			return e
		}
	}
	return nil
}

// actionOrList decodes the two shapes changing calls answer with.
type actionOrList struct {
	Action  *Action  `json:"action"`
	Actions []Action `json:"actions"`
}

func (a actionOrList) all() []Action {
	out := append([]Action(nil), a.Actions...)
	if a.Action != nil {
		out = append(out, *a.Action)
	}
	return out
}

// doAction sends a changing request and waits for the actions it started.
func (c *Client) doAction(ctx context.Context, method, path string, in any) error {
	var body actionOrList
	// Some calls answer 204 without a body: nothing to wait for.
	if err := c.do(ctx, method, path, in, &body); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return c.WaitActions(ctx, body.all()...)
}
