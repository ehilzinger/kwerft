// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package logs reads container logs from VictoriaLogs, which Vector fills
// from every node (install.sh stage "Observability").
//
// Confinement. VictoriaLogs has no authentication and holds every
// namespace's logs, so the console must decide what a query may see. Every
// request carries a Scope: the namespaces (and, optionally, app/build/task
// labels) it is confined to. The scope is sent as VictoriaLogs'
// extra_filters and extra_stream_filters query args, which VictoriaLogs
// applies to the parsed query as a whole (and to every subquery), so no text
// in the user's LogsQL can widen it: "a OR *" or "namespace:other" only
// narrow it further. On top of that, the user's text must be a filter: pipes
// (which could reshape the output) and unterminated quotes are refused, and
// the console appends its own pipes. A request without namespaces is never
// sent: Query returns nothing and Tail refuses it.
//
// Bounds: a time range of at most Limits.MaxRange, at most Limits.MaxLimit
// lines, Limits.MaxBytes of response, Limits.MaxLine bytes per line, and a
// timeout per query. VictoriaLogs being unreachable or failing is
// ErrUnavailable; a query it cannot parse is a *QueryError.
package logs

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Limits bound what one request may ask of VictoriaLogs.
type Limits struct {
	DefaultLimit int           // lines when the request names none
	MaxLimit     int           // lines per query
	MaxRange     time.Duration // between start and end
	MaxBytes     int64         // of one query's response
	MaxLine      int           // bytes of a line's text; longer ones are cut
	MaxRecord    int           // bytes of one record from VictoriaLogs; longer ones are skipped
	MaxQuery     int           // bytes of the user's LogsQL
	Timeout      time.Duration // per query
}

// DefaultLimits suit the console: a page of lines, a month of history (the
// installer keeps 14 days), and answers within seconds.
var DefaultLimits = Limits{
	DefaultLimit: 200, MaxLimit: 5000, MaxRange: 31 * 24 * time.Hour,
	MaxBytes: 16 << 20, MaxLine: 16 << 10, MaxRecord: 1 << 20, MaxQuery: 4 << 10,
	Timeout: 20 * time.Second,
}

// ErrUnavailable means VictoriaLogs did not answer, or failed.
var ErrUnavailable = errors.New("log history (VictoriaLogs) is not available")

// QueryError is a query VictoriaLogs (or the console) refuses; Msg says why
// in words meant for the person who wrote it.
type QueryError struct{ Msg string }

func (e *QueryError) Error() string { return e.Msg }

// Scope confines a request. Namespaces must not be empty. Fields are exact
// matches on stream fields (app, build, task, pod, container, project), all
// of which must hold.
type Scope struct {
	Namespaces []string
	Fields     map[string]string
}

// Request is one query.
type Request struct {
	// Query is the user's LogsQL filter; empty means every line.
	Query string
	Scope Scope
	// Level is "", "warn" (warnings and errors) or "error".
	Level      string
	Start, End time.Time
	// Limit is the number of newest lines to return; 0 means DefaultLimit.
	Limit int
}

// Entry is one log line.
type Entry struct {
	Time      time.Time `json:"time"`
	Namespace string    `json:"namespace"`
	Project   string    `json:"project,omitempty"`
	App       string    `json:"app,omitempty"`
	Build     string    `json:"build,omitempty"`
	Task      string    `json:"task,omitempty"`
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	Stream    string    `json:"stream,omitempty"`
	Level     string    `json:"level,omitempty"`
	Line      string    `json:"line"`
	Truncated bool      `json:"truncated,omitempty"`
	StreamID  string    `json:"-"`
}

// Result is a query's answer, oldest line first. Truncated means there are
// older lines than the first one returned (the limit or the byte cap was
// reached).
type Result struct {
	Entries   []Entry
	Truncated bool
}

// Client talks to VictoriaLogs' HTTP API.
type Client struct {
	base   string
	http   *http.Client
	Limits Limits
}

// New returns a client for VictoriaLogs at baseURL (observability.LogsURL).
func New(baseURL string) *Client {
	tr := &http.Transport{
		Proxy:                 nil, // in-cluster only
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: &http.Client{Transport: tr}, Limits: DefaultLimits}
}

// NewWithHTTP is New with the HTTP client given: VictoriaLogs of a remote
// cluster, reached through its API server's service proxy with that
// client's credentials (observability.ServiceProxyURL). The client must not
// have an overall timeout, since tails last as long as their context.
func NewWithHTTP(baseURL string, hc *http.Client) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), http: hc, Limits: DefaultLimits}
}

// levelFilters are fixed LogsQL filters per level: a structured level field
// when the line was JSON, otherwise the usual words in the text.
var levelFilters = map[string]string{
	"error": `(level:in(error,err,fatal,critical,crit,panic,emerg,emergency,alert) OR (level:"" AND (i(error) OR i(fatal) OR i(panic) OR i(exception) OR i(traceback))))`,
	"warn":  `(level:in(warn,warning,error,err,fatal,critical,crit,panic,emerg,emergency,alert) OR (level:"" AND (i(warn) OR i(warning) OR i(error) OR i(fatal) OR i(panic) OR i(exception) OR i(traceback))))`,
}

// ValidLevel reports whether Request.Level accepts l.
func ValidLevel(l string) bool { _, ok := levelFilters[l]; return l == "" || ok }

// CheckQuery refuses what a user's LogsQL may not contain: pipes (outside
// quotes), unterminated quotes, control characters other than tabs and line
// breaks, and more than max bytes.
func CheckQuery(q string, max int) error {
	if len(q) > max {
		return &QueryError{Msg: fmt.Sprintf("The query is too long: at most %d characters.", max)}
	}
	if !utf8.ValidString(q) {
		return &QueryError{Msg: "The query is not valid text."}
	}
	var quote rune
	escaped := false
	for _, r := range q {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0x7f {
			return &QueryError{Msg: "The query contains a control character."}
		}
		switch {
		case quote != 0 && escaped:
			escaped = false
		case quote != 0 && r == '\\' && quote != '`':
			escaped = true
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
		case r == '"' || r == '\'' || r == '`':
			quote = r
		case r == '|':
			return &QueryError{Msg: "Enter filters only, like error or status:500: pipes (|) are not supported here."}
		}
	}
	if quote != 0 {
		return &QueryError{Msg: fmt.Sprintf("A quote (%c) is not closed.", quote)}
	}
	return nil
}

// errNoScope guards against ever sending an unconfined request.
var errNoScope = errors.New("logs: a request needs at least one namespace")

// confinement is the JSON for extra_filters / extra_stream_filters.
func confinement(s Scope) (string, error) {
	ns := slices.Compact(slices.Sorted(slices.Values(s.Namespaces)))
	ns = slices.DeleteFunc(ns, func(n string) bool { return n == "" })
	if len(ns) == 0 {
		return "", errNoScope
	}
	m := map[string]any{FieldNamespace: ns}
	for k, v := range s.Fields {
		if !streamFields[k] || k == FieldNamespace {
			return "", fmt.Errorf("logs: %q cannot confine a request", k)
		}
		m[k] = v
	}
	b, err := json.Marshal(m)
	return string(b), err
}

// params builds the query args shared by query and tail. pipes follow the
// user's filter, on a line of their own so a comment (#) in the filter
// cannot swallow them.
func (c *Client) params(req Request, pipes string) (url.Values, error) {
	if err := CheckQuery(req.Query, c.Limits.MaxQuery); err != nil {
		return nil, err
	}
	if !ValidLevel(req.Level) {
		return nil, &QueryError{Msg: "Level is error, warn or empty."}
	}
	conf, err := confinement(req.Scope)
	if err != nil {
		return nil, err
	}
	filter := strings.TrimSpace(req.Query)
	if filter == "" {
		filter = "*"
	}
	v := url.Values{}
	v.Set("query", filter+"\n| fields "+strings.Join(selected, ", ")+pipes)
	v.Add("extra_stream_filters", conf)
	v.Add("extra_filters", conf)
	if f := levelFilters[req.Level]; f != "" {
		v.Add("extra_filters", f)
	}
	return v, nil
}

// Query returns the newest lines matching req in its time range, oldest
// first. An empty scope returns nothing.
func (c *Client) Query(ctx context.Context, req Request) (Result, error) {
	limit := req.Limit
	if limit <= 0 {
		limit = c.Limits.DefaultLimit
	}
	limit = min(limit, c.Limits.MaxLimit)
	end := req.End
	if end.IsZero() {
		end = time.Now()
	}
	start := req.Start
	if start.IsZero() || end.Sub(start) > c.Limits.MaxRange {
		start = end.Add(-c.Limits.MaxRange)
	}
	if !start.Before(end) {
		return Result{}, &QueryError{Msg: "The start of the time range must be before its end."}
	}
	// One more than asked, to know whether older lines exist.
	v, err := c.params(req, fmt.Sprintf("\n| sort by (_time desc) limit %d", limit+1))
	if errors.Is(err, errNoScope) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	v.Set("start", start.UTC().Format(time.RFC3339Nano))
	v.Set("end", end.UTC().Format(time.RFC3339Nano))
	v.Set("timeout", c.Limits.Timeout.String())

	ctx, cancel := context.WithTimeout(ctx, c.Limits.Timeout+5*time.Second)
	defer cancel()
	body, err := c.do(ctx, "/select/logsql/query", v)
	if err != nil {
		return Result{}, err
	}
	defer body.Close()

	var res Result
	rd := newRecordReader(io.LimitReader(body, c.Limits.MaxBytes), c.Limits)
	for {
		e, err := rd.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil && !errors.Is(ctx.Err(), context.Canceled) {
				return Result{}, fmt.Errorf("%w: no answer within %s", ErrUnavailable, c.Limits.Timeout)
			}
			if ctx.Err() != nil {
				return Result{}, ctx.Err()
			}
			return Result{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		res.Entries = append(res.Entries, e)
	}
	if rd.read >= c.Limits.MaxBytes {
		res.Truncated = true // the byte cap cut the answer; the newest lines came first
	}
	// Newest first from VictoriaLogs; stable for equal times.
	slices.SortStableFunc(res.Entries, func(x, y Entry) int { return y.Time.Compare(x.Time) })
	if len(res.Entries) > limit {
		res.Entries, res.Truncated = res.Entries[:limit], true
	}
	slices.Reverse(res.Entries)
	return res, nil
}

// Tail follows new lines matching req (its time range is ignored) until ctx
// ends or VictoriaLogs closes the stream. It returns once VictoriaLogs has
// accepted the query, so a refusal is an error here, not a stream event.
func (c *Client) Tail(ctx context.Context, req Request) (*Tail, error) {
	v, err := c.params(req, "")
	if errors.Is(err, errNoScope) {
		return nil, &QueryError{Msg: "There are no projects to read logs from."}
	}
	if err != nil {
		return nil, err
	}
	body, err := c.do(ctx, "/select/logsql/tail", v)
	if err != nil {
		return nil, err
	}
	// The body is never bounded in total; each record is.
	return &Tail{body: body, rd: newRecordReader(body, c.Limits)}, nil
}

// Tail is a live stream of lines.
type Tail struct {
	body io.ReadCloser
	rd   *recordReader
}

// Next blocks for the next line. io.EOF means VictoriaLogs ended the stream.
func (t *Tail) Next() (Entry, error) { return t.rd.next() }

// Close ends the stream; a blocked Next returns.
func (t *Tail) Close() error { return t.body.Close() }

func (c *Client) do(ctx context.Context, path string, v url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(v.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if res.StatusCode == http.StatusOK {
		return res.Body, nil
	}
	defer res.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(res.Body, 2<<10))
	text := strings.TrimSpace(string(msg))
	if res.StatusCode == http.StatusBadRequest {
		return nil, &QueryError{Msg: "VictoriaLogs cannot run this query: " + queryProblem(text)}
	}
	return nil, fmt.Errorf("%w: HTTP %d: %s", ErrUnavailable, res.StatusCode, text)
}

// queryProblem shortens VictoriaLogs' parse error ("cannot parse query
// [...]: ...") to what helps.
func queryProblem(text string) string {
	if i := strings.LastIndex(text, "]: "); i >= 0 && strings.Contains(text, "cannot parse query") {
		text = text[i+3:]
	}
	if len(text) > 300 {
		text = text[:300] + "…"
	}
	if text == "" {
		text = "it is not valid LogsQL"
	}
	return text
}

// ---- records -------------------------------------------------------------------

// record is one JSON line from VictoriaLogs; every value is a string.
type record struct {
	Time      string `json:"_time"`
	Msg       string `json:"_msg"`
	StreamID  string `json:"_stream_id"`
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Stream    string `json:"stream"`
	Project   string `json:"project"`
	App       string `json:"app"`
	Build     string `json:"build"`
	Task      string `json:"task"`
	Level     string `json:"level"`
}

type recordReader struct {
	br   *bufio.Reader
	lim  Limits
	read int64
	buf  []byte
}

func newRecordReader(r io.Reader, lim Limits) *recordReader {
	return &recordReader{br: bufio.NewReaderSize(r, 64<<10), lim: lim}
}

// next returns the next record as an Entry. Records longer than MaxRecord
// and lines that are not JSON are skipped.
func (r *recordReader) next() (Entry, error) {
	for {
		r.buf = r.buf[:0]
		tooLong := false
		var err error
		for {
			var chunk []byte
			chunk, err = r.br.ReadSlice('\n')
			r.read += int64(len(chunk))
			if !tooLong {
				if len(r.buf)+len(chunk) > r.lim.MaxRecord {
					tooLong = true
				} else {
					r.buf = append(r.buf, chunk...)
				}
			}
			if !errors.Is(err, bufio.ErrBufferFull) {
				break
			}
		}
		line := bytes.TrimSpace(r.buf)
		if len(line) > 0 && !tooLong {
			if e, ok := r.entry(line); ok {
				return e, nil
			}
		}
		if err != nil {
			return Entry{}, err // io.EOF at the end
		}
	}
}

func (r *recordReader) entry(line []byte) (Entry, bool) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Entry{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, rec.Time)
	if err != nil {
		return Entry{}, false
	}
	e := Entry{
		Time: at.UTC(), Namespace: rec.Namespace, Project: rec.Project, App: rec.App, Build: rec.Build, Task: rec.Task,
		Pod: rec.Pod, Container: rec.Container, Stream: rec.Stream, Level: rec.Level, Line: rec.Msg, StreamID: rec.StreamID,
	}
	if len(e.Line) > r.lim.MaxLine {
		cut := r.lim.MaxLine
		for cut > 0 && !utf8.RuneStart(e.Line[cut]) {
			cut--
		}
		e.Line, e.Truncated = e.Line[:cut], true
	}
	return e, true
}

// ParseTime reads a time for since/until: a number of seconds ago, a Go
// duration ago ("15m", "24h"), or an RFC 3339 timestamp.
func ParseTime(v string, now time.Time) (time.Time, error) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
		return now.Add(-time.Duration(n) * time.Second), nil
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("%q is not a time: use seconds or a duration ago (3600, 15m, 24h) or a timestamp (2026-10-04T09:00:00Z)", v)
}
