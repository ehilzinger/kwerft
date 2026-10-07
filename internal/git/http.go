// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is a Git host's answer other than success. It wraps one of the
// Err* values for 401, 403 and 404.
type APIError struct {
	Method, Path string
	Status       int
	Message      string // the host's own message, if it sent one
	kind         error
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (e *APIError) Unwrap() error { return e.kind }

// apiClient sends JSON requests to one REST API.
type apiClient struct {
	http *http.Client
	base string // e.g. https://api.github.com
	// auth sets the credentials on a request; it may fetch them first (a
	// GitHub App installation token).
	auth   func(ctx context.Context, req *http.Request) error
	header map[string]string
}

// maxResponse bounds what is read from a host, so a misbehaving one cannot
// exhaust memory.
const maxResponse = 4 << 20

func (c *apiClient) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.request(ctx, method, path, body, out)
	return err
}

func (c *apiClient) request(ctx context.Context, method, path string, body, out any) (int, error) {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "kwerft")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	if c.auth != nil {
		if err := c.auth(ctx, req); err != nil {
			return 0, err
		}
	}
	// The path without a query: queries may carry refs, never credentials,
	// but keep errors short.
	shown, _, _ := strings.Cut(path, "?")
	res, err := c.http.Do(req)
	if err != nil {
		// url.Error repeats the URL, which has no credentials in it.
		return 0, fmt.Errorf("%s %s: %w", method, shown, unwrapURLError(err))
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return res.StatusCode, fmt.Errorf("%s %s: %w", method, shown, err)
	}
	if res.StatusCode >= 300 {
		e := &APIError{Method: method, Path: shown, Status: res.StatusCode, Message: hostMessage(raw)}
		switch res.StatusCode {
		case http.StatusUnauthorized:
			e.kind = ErrUnauthorized
		case http.StatusForbidden:
			e.kind = ErrForbidden
		case http.StatusNotFound:
			e.kind = ErrNotFound
		}
		return res.StatusCode, e
	}
	if out != nil && len(raw) > 0 && method != http.MethodHead {
		if err := json.Unmarshal(raw, out); err != nil {
			return res.StatusCode, fmt.Errorf("%s %s: unexpected answer: %w", method, shown, err)
		}
	}
	return res.StatusCode, nil
}

// hostMessage extracts the message GitHub, GitLab and Gitea put in error
// bodies ({"message": ...} or {"error": ...}).
func hostMessage(raw []byte) string {
	var body struct {
		Message any `json:"message"`
		Error   any `json:"error"`
	}
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	for _, v := range []any{body.Message, body.Error} {
		switch m := v.(type) {
		case string:
			return shortLine(m, 300)
		case nil:
		default:
			if b, err := json.Marshal(m); err == nil {
				return shortLine(string(b), 300)
			}
		}
	}
	return ""
}

func unwrapURLError(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok && u.Unwrap() != nil {
		return u.Unwrap()
	}
	return err
}
