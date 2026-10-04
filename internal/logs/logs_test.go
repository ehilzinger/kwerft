package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeVL is a VictoriaLogs stand-in: it records each request's form and
// answers with the configured status and body.
type fakeVL struct {
	mu     sync.Mutex
	forms  []url.Values
	paths  []string
	status int
	body   string
}

func (f *fakeVL) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	f.mu.Lock()
	f.forms = append(f.forms, r.PostForm)
	f.paths = append(f.paths, r.URL.Path)
	status, body := f.status, f.body
	f.mu.Unlock()
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeVL) last(t *testing.T) url.Values {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.forms) == 0 {
		t.Fatal("VictoriaLogs got no request")
	}
	return f.forms[len(f.forms)-1]
}

func vlRecord(at time.Time, pod, msg string) string {
	b, _ := json.Marshal(map[string]string{
		"_time": at.Format(time.RFC3339Nano), "_msg": msg, "_stream_id": "s1",
		"namespace": "shop", "pod": pod, "container": "app", "stream": "stdout", "app": "web", "project": "shop",
	})
	return string(b) + "\n"
}

func newFake(t *testing.T) (*fakeVL, *Client) {
	f := &fakeVL{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, New(srv.URL)
}

func TestQueryConfinesAndOrders(t *testing.T) {
	f, c := newFake(t)
	t0 := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	// Newest first, as the sort pipe returns them; one more than the limit.
	f.body = vlRecord(t0.Add(3*time.Second), "web-1", "four") + vlRecord(t0.Add(2*time.Second), "web-2", "three") +
		"not json\n" + vlRecord(t0.Add(time.Second), "web-1", "two") + vlRecord(t0, "web-1", "one")
	res, err := c.Query(context.Background(), Request{
		Query: `namespace:kwerft-builds OR * OR {namespace="kube-system"}`,
		Scope: Scope{Namespaces: []string{"shop", "blog", "shop"}, Fields: map[string]string{FieldApp: "web"}},
		Level: "error", Start: t0.Add(-time.Hour), End: t0.Add(time.Hour), Limit: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range res.Entries {
		got = append(got, e.Line)
	}
	if strings.Join(got, ",") != "two,three,four" || !res.Truncated {
		t.Errorf("entries = %v truncated=%v, want two,three,four and truncated", got, res.Truncated)
	}
	if e := res.Entries[0]; e.Namespace != "shop" || e.Pod != "web-1" || e.App != "web" || e.Container != "app" || !e.Time.Equal(t0.Add(time.Second)) {
		t.Errorf("entry = %+v", e)
	}

	form := f.last(t)
	conf := `{"app":"web","namespace":["blog","shop"]}`
	if got := form["extra_stream_filters"]; len(got) != 1 || got[0] != conf {
		t.Errorf("extra_stream_filters = %q, want %s", got, conf)
	}
	if got := form["extra_filters"]; len(got) != 2 || got[0] != conf || got[1] != levelFilters["error"] {
		t.Errorf("extra_filters = %q", got)
	}
	// The user's text is only ever the query's filter; the console's pipes
	// follow on their own line.
	want := "namespace:kwerft-builds OR * OR {namespace=\"kube-system\"}\n| fields " + strings.Join(selected, ", ") + "\n| sort by (_time desc) limit 4"
	if form.Get("query") != want {
		t.Errorf("query = %q, want %q", form.Get("query"), want)
	}
	if form.Get("start") != "2026-10-04T08:00:00Z" || form.Get("end") != "2026-10-04T10:00:00Z" || form.Get("timeout") != "20s" {
		t.Errorf("range = %s .. %s timeout %s", form.Get("start"), form.Get("end"), form.Get("timeout"))
	}
}

func TestQueryWithoutScopeIsNeverSent(t *testing.T) {
	f, c := newFake(t)
	res, err := c.Query(context.Background(), Request{Query: "*", Scope: Scope{Namespaces: []string{""}}})
	if err != nil || len(res.Entries) != 0 {
		t.Fatalf("empty scope: %v %+v", err, res)
	}
	if _, err := c.Tail(context.Background(), Request{}); err == nil {
		t.Error("tail without a scope was accepted")
	}
	if len(f.forms) != 0 {
		t.Errorf("VictoriaLogs was asked %d times", len(f.forms))
	}
	// Only stream fields confine, and never namespace through Fields.
	for _, k := range []string{FieldNamespace, FieldLevel, "_msg"} {
		_, err := c.Query(context.Background(), Request{Scope: Scope{Namespaces: []string{"shop"}, Fields: map[string]string{k: "x"}}})
		if err == nil {
			t.Errorf("field %q was accepted as confinement", k)
		}
	}
}

func TestCheckQuery(t *testing.T) {
	ok := []string{"", "error", `"a | b"`, `'x|y' status:500`, "`a|b`", `"esc \" | still quoted"`, "a\tb\nc # comment"}
	for _, q := range ok {
		if err := CheckQuery(q, 100); err != nil {
			t.Errorf("CheckQuery(%q) = %v", q, err)
		}
	}
	bad := []string{"error | stats count()", `pod:in(* | fields pod)`, `"open`, "x\x00", strings.Repeat("a", 101), "* | union (namespace:x)"}
	for _, q := range bad {
		var qe *QueryError
		if err := CheckQuery(q, 100); !errors.As(err, &qe) {
			t.Errorf("CheckQuery(%q) = %v, want a QueryError", q, err)
		}
	}
}

func TestQueryErrors(t *testing.T) {
	f, c := newFake(t)
	scope := Scope{Namespaces: []string{"shop"}}
	f.status, f.body = http.StatusBadRequest, "cannot parse query [foo:(]: missing ')'"
	_, err := c.Query(context.Background(), Request{Query: "foo:(", Scope: scope})
	var qe *QueryError
	if !errors.As(err, &qe) || !strings.Contains(qe.Msg, "missing ')'") {
		t.Errorf("bad query: %v", err)
	}
	f.status, f.body = http.StatusInternalServerError, "boom"
	if _, err := c.Query(context.Background(), Request{Scope: scope}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("500: %v", err)
	}
	down := New("http://127.0.0.1:1")
	if _, err := down.Query(context.Background(), Request{Scope: scope}); !errors.Is(err, ErrUnavailable) {
		t.Errorf("unreachable: %v", err)
	}
	if _, err := c.Query(context.Background(), Request{Scope: scope, Level: "debug"}); !errors.As(err, &qe) {
		t.Errorf("bad level: %v", err)
	}
}

func TestQueryCaps(t *testing.T) {
	f, c := newFake(t)
	c.Limits.MaxLine = 10
	c.Limits.MaxBytes = 400
	t0 := time.Now().Add(-time.Minute)
	var body strings.Builder
	for i := range 20 {
		body.WriteString(vlRecord(t0.Add(-time.Duration(i)*time.Second), "p", fmt.Sprintf("line %d: %s", i, strings.Repeat("é", 10))))
	}
	f.body = body.String()
	res, err := c.Query(context.Background(), Request{Scope: Scope{Namespaces: []string{"shop"}}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Entries) == 0 || len(res.Entries) >= 20 {
		t.Fatalf("byte cap: %d entries, truncated=%v", len(res.Entries), res.Truncated)
	}
	for _, e := range res.Entries {
		if len(e.Line) > 10 || !e.Truncated || !strings.HasPrefix(e.Line, "line ") {
			t.Errorf("line not cut at a rune: %q", e.Line)
		}
	}
	// The newest came first, so they are the ones kept; oldest first in the result.
	if last := res.Entries[len(res.Entries)-1]; !strings.HasPrefix(last.Line, "line 0") {
		t.Errorf("newest kept = %q", last.Line)
	}
	// A huge range is clamped to MaxRange.
	_, _ = c.Query(context.Background(), Request{Scope: Scope{Namespaces: []string{"shop"}}, Start: time.Unix(0, 0)})
	start, _ := time.Parse(time.RFC3339Nano, f.last(t).Get("start"))
	if time.Since(start) > c.Limits.MaxRange+time.Minute {
		t.Errorf("range not clamped: start %s", start)
	}
}

func TestTail(t *testing.T) {
	t0 := time.Now()
	forms := make(chan url.Values, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		forms <- r.PostForm
		if r.URL.Path != "/select/logsql/tail" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, vlRecord(t0, "web-1", "hello"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := New(srv.URL)
	tail, err := c.Tail(context.Background(), Request{Query: "hello", Scope: Scope{Namespaces: []string{"shop"}}})
	if err != nil {
		t.Fatal(err)
	}
	e, err := tail.Next()
	if err != nil || e.Line != "hello" || e.Pod != "web-1" {
		t.Fatalf("next: %+v %v", e, err)
	}
	done := make(chan error)
	go func() { _, err := tail.Next(); done <- err }()
	_ = tail.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Next after Close returned a line")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not unblock Next")
	}
	if form := <-forms; form.Get("extra_filters") != `{"namespace":["shop"]}` || strings.Contains(form.Get("query"), "sort") {
		t.Errorf("tail form = %v", form)
	}
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{
		"3600": now.Add(-time.Hour), "15m": now.Add(-15 * time.Minute), "2026-10-04T09:00:00Z": time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC),
	} {
		if got, err := ParseTime(in, now); err != nil || !got.Equal(want) {
			t.Errorf("ParseTime(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseTime("yesterday", now); err == nil {
		t.Error("yesterday parsed")
	}
}
