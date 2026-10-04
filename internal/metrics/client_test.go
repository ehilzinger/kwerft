package metrics

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeVM answers like VictoriaMetrics and records what it was asked.
type fakeVM struct {
	status int
	body   string
	calls  []*http.Request
	forms  []url.Values
}

func (f *fakeVM) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		f.calls = append(f.calls, r)
		f.forms = append(f.forms, form)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		_, _ = io.WriteString(w, f.body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

const matrix = `{"status":"success","data":{"resultType":"matrix","result":[
 {"metric":{"namespace":"shop","app":"api"},"values":[[1700000000,"1.5"],[1700000030,"NaN"],[1700000060,"2"]]}]}}`

func TestConfinementGoesIntoTheURL(t *testing.T) {
	vm := &fakeVM{status: 200, body: matrix}
	c := New(vm.server(t).URL)
	r, _ := NewRange(time.Unix(1700003600, 0), time.Hour, 0)
	series, err := c.QueryRange(context.Background(), `up{namespace="kube-system"}`, r, Namespaces("shop", "blog"))
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || len(series[0].Points) != 2 || series[0].Points[1].V != 2 {
		t.Fatalf("series = %+v (NaN must be skipped)", series)
	}
	got := vm.calls[0].URL.Query()["extra_filters[]"]
	if len(got) != 1 || got[0] != `{namespace=~"blog|shop"}` {
		t.Fatalf("extra_filters[] = %q", got)
	}
	if vm.forms[0].Get("query") != `up{namespace="kube-system"}` || vm.forms[0].Has("extra_filters[]") {
		t.Fatalf("form = %v", vm.forms[0])
	}
	if vm.forms[0].Get("step") != "30s" || vm.forms[0].Get("timeout") == "" {
		t.Fatalf("form = %v", vm.forms[0])
	}
}

func TestUnconfinedSendsNoFilter(t *testing.T) {
	vm := &fakeVM{status: 200, body: `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3"]}]}}`}
	c := New(vm.server(t).URL)
	series, err := c.Query(context.Background(), "count(up)", time.Unix(1700000000, 0), Unconfined())
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 1 || series[0].Points[0] != (Point{T: 1700000000, V: 3}) {
		t.Fatalf("series = %+v", series)
	}
	if vm.calls[0].URL.RawQuery != "" {
		t.Fatalf("query string = %q", vm.calls[0].URL.RawQuery)
	}
}

func TestEmptyScopeNeverAsks(t *testing.T) {
	vm := &fakeVM{status: 200, body: matrix}
	c := New(vm.server(t).URL)
	for _, scope := range []Scope{{}, Namespaces(), Namespaces("kube-system", "kwerft-observability", "Not A Label")} {
		series, err := c.Query(context.Background(), `{__name__=~".+"}`, time.Now(), scope)
		if err != nil || len(series) != 0 {
			t.Fatalf("scope %+v: %v %v", scope, series, err)
		}
	}
	if len(vm.calls) != 0 {
		t.Fatalf("VictoriaMetrics was asked %d times", len(vm.calls))
	}
}

func TestErrors(t *testing.T) {
	for _, tc := range []struct {
		status      int
		body        string
		unavailable bool
	}{
		{422, `{"status":"error","errorType":"422","error":"cannot parse query"}`, false},
		{400, `{"status":"error","error":"bad"}`, false},
		{503, `{"status":"error","error":"cannot execute query: timeout exceeded"}`, false},
		{503, `{"status":"error","error":"too many concurrent requests"}`, true},
		{502, `<html>bad gateway</html>`, true},
		{200, `not json`, true},
	} {
		vm := &fakeVM{status: tc.status, body: tc.body}
		_, err := New(vm.server(t).URL).Query(context.Background(), "up", time.Now(), Unconfined())
		var qe *QueryError
		if got := errors.Is(err, ErrUnavailable); got != tc.unavailable || (!got && !errors.As(err, &qe)) {
			t.Errorf("HTTP %d %s: err = %v", tc.status, tc.body, err)
		}
	}
	// Nothing listening.
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	if _, err := New(srv.URL).Query(context.Background(), "up", time.Now(), Unconfined()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("closed server: %v", err)
	}
}

func TestNewRange(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		span, step time.Duration
		want       time.Duration
	}{
		{time.Hour, 0, 30 * time.Second},
		{6 * time.Hour, 0, 2 * time.Minute},
		{24 * time.Hour, 0, 10 * time.Minute},
		{7 * 24 * time.Hour, 0, time.Hour},
		{30 * 24 * time.Hour, 0, 3 * time.Hour},
		{30 * 24 * time.Hour, time.Second, 2 * time.Hour}, // raised to stay within MaxPoints
		{time.Hour, time.Second, 30 * time.Second},
		{time.Hour, 5 * time.Minute, 5 * time.Minute},
	} {
		r, err := NewRange(now, tc.span, tc.step)
		if err != nil {
			t.Fatal(err)
		}
		if r.Step != tc.want || r.Points() > MaxPoints || !r.End.Equal(now) || r.End.Sub(r.Start) != tc.span {
			t.Errorf("span %s step %s: %+v (%d points), want step %s", tc.span, tc.step, r, r.Points(), tc.want)
		}
	}
	for _, span := range []time.Duration{time.Minute, 31 * 24 * time.Hour} {
		if _, err := NewRange(now, span, 0); err == nil {
			t.Errorf("span %s accepted", span)
		}
	}
}

func TestParseDuration(t *testing.T) {
	for in, want := range map[string]time.Duration{"90s": 90 * time.Second, "15m": 15 * time.Minute, "6h": 6 * time.Hour, "7d": 7 * 24 * time.Hour, "2w": 14 * 24 * time.Hour} {
		if got, err := ParseDuration(in); err != nil || got != want {
			t.Errorf("%s: %s %v", in, got, err)
		}
	}
	for _, in := range []string{"", "1", "1y", "-1h", "0h", "1.5h", "1h30m", "999999h"} {
		if _, err := ParseDuration(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestAppQueries(t *testing.T) {
	q, err := AppQueries("shop", "api", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range AppSeries {
		if !strings.Contains(q[k], `namespace="shop"`) {
			t.Errorf("%s: %s", k, q[k])
		}
	}
	if len(q) != len(AppSeries) {
		t.Errorf("queries %v, series %v", q, AppSeries)
	}
	for _, bad := range [][2]string{{`shop"}`, "api"}, {"shop", `api"} or up{`}, {"Shop", "api"}, {"", "api"}} {
		if _, err := AppQueries(bad[0], bad[1], time.Minute); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The catalog reads the chart's recording rules; their names must match.
func TestRecordsAreInTheChart(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kwerft", "templates", "metrics-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range Records {
		if !strings.Contains(string(raw), "record: "+r+"\n") {
			t.Errorf("recording rule %s not in metrics-rules.yaml", r)
		}
	}
}

func TestPlatformNamespaces(t *testing.T) {
	for _, ns := range []string{"kube-system", "kube-flannel", "kwerft-system", "kwerft-anything", "traefik", "cert-manager", "default"} {
		if !IsPlatformNamespace(ns) {
			t.Errorf("%s is platform", ns)
		}
	}
	if IsPlatformNamespace("shop") {
		t.Error("shop is a project")
	}
	if f := Namespaces("shop", "shop", "kube-system").Filter(); f != `{namespace=~"shop"}` {
		t.Errorf("filter %s", f)
	}
}
