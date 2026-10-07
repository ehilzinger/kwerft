// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	authzv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// setupAlerting creates kwerft-observability and the stack's VMAlertmanager
// (with the namespace matcher off, as install.sh sets it).
func setupAlerting(ctx context.Context, c client.Client) error {
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: observability.Namespace}}); err != nil {
		return err
	}
	am := &unstructured.Unstructured{}
	am.SetGroupVersionKind(controllers.VMAlertmanagerGVK)
	am.SetNamespace(observability.Namespace)
	am.SetName("vm-victoria-metrics-k8s-stack")
	am.Object["spec"] = map[string]any{"disableNamespaceMatcher": true}
	return c.Create(ctx, am)
}

// fakeAlertmanager serves the parts of Alertmanager's API v2 the console uses.
type fakeAlertmanager struct {
	mu       sync.Mutex
	alerts   []alerting.AMAlert
	silences map[string]alerting.Silence
	filters  []string
	down     bool
	next     int
}

func newFakeAlertmanager(t *testing.T) (*fakeAlertmanager, *httptest.Server) {
	f := &fakeAlertmanager{silences: map[string]alerting.Silence{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.down {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/silence/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/alerts":
			f.filters = append(f.filters, r.URL.Query().Get("filter"))
			writeJSON(w, http.StatusOK, f.alerts)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v2/silences":
			out := []alerting.Silence{}
			for _, s := range f.silences {
				out = append(out, s)
			}
			writeJSON(w, http.StatusOK, out)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v2/silences":
			var s alerting.Silence
			_ = json.NewDecoder(r.Body).Decode(&s)
			f.next++
			s.ID = "s-new-" + strconv.Itoa(f.next)
			f.silences[s.ID] = s
			writeJSON(w, http.StatusOK, map[string]string{"silenceID": s.ID})
		case r.Method == http.MethodGet && id != r.URL.Path:
			s, ok := f.silences[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeJSON(w, http.StatusOK, s)
		case r.Method == http.MethodDelete && id != r.URL.Path:
			if _, ok := f.silences[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			delete(f.silences, id)
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAlertmanager) set(alerts ...alerting.AMAlert) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range alerts {
		if alerts[i].Fingerprint == "" {
			alerts[i].Fingerprint = alerting.Fingerprint(alerts[i].Labels)
		}
	}
	f.alerts = alerts
}

// fakeMetrics answers the two ALERTS queries of the resolved list.
func fakeMetrics(t *testing.T, last, first []map[string]any) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if !strings.Contains(q, `ALERTS{alertstate="firing",kwerft_rule!=""}[24h]`) {
			t.Errorf("unexpected query %q", q)
		}
		result := last
		if strings.HasPrefix(q, "tfirst_over_time") {
			result = first
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": result}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func series(labels map[string]string, v int64) map[string]any {
	m := map[string]any{"__name__": "ALERTS", "alertstate": "firing"}
	for k, val := range labels {
		m[k] = val
	}
	return map[string]any{"metric": m, "value": []any{float64(v), strconv.FormatInt(v, 10)}}
}

type alertsEnv struct {
	*console
	am   *fakeAlertmanager
	logs *lockedBuffer
	sys  client.Client
}

func newAlertsConsole(t *testing.T, hooks ...func(*alertsAPI)) *alertsEnv {
	t.Helper()
	requireCluster(t)
	am, amSrv := newFakeAlertmanager(t)
	sys, err := client.New(cluster.console, client.Options{Scheme: cluster.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuffer{}
	c := newConsole(t, func(cfg *Config) {
		cfg.ConsoleDomain = "console.example.com"
		cfg.Logger = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		cfg.System, cfg.SystemReader = sys, sys
		cfg.alertsHook = func(al *alertsAPI) {
			al.am = &alerting.Alertmanager{URL: amSrv.URL}
			al.credentialsWait = 15 * time.Second
			for _, h := range hooks {
				h(al)
			}
		}
	})
	return &alertsEnv{console: c, am: am, logs: logs, sys: sys}
}

func amAlert(labels map[string]string, summary string, silencedBy ...string) alerting.AMAlert {
	a := alerting.AMAlert{Labels: labels, Annotations: map[string]string{"summary": summary, "description": "d", "console_url": "https://console.example.com/x"},
		StartsAt: time.Now().Add(-time.Minute).UTC().Truncate(time.Second)}
	a.Status.State = "active"
	if len(silencedBy) > 0 {
		a.Status.State, a.Status.SilencedBy = "suppressed", silencedBy
	}
	return a
}

func alertsOf(t *testing.T, s *session, query string) []alertJSON {
	t.Helper()
	var out []alertJSON
	if code := s.do(t, "GET", "/api/v1/alerts"+query, nil, &out); code != http.StatusOK {
		t.Fatalf("GET alerts%s: %d", query, code)
	}
	return out
}

func rulesOfAlerts(alerts []alertJSON) []string {
	var out []string
	for _, a := range alerts {
		out = append(out, a.Rule+"/"+a.State)
	}
	slices.Sort(out)
	return out
}

func TestAlertsAreConfinedToProjects(t *testing.T) {
	now := time.Now()
	resolvedLabels := map[string]string{"alertname": "restarts", "kwerft_rule": "restarts", "severity": "warning", "namespace": "alertshop", "app": "web"}
	firingLabels := map[string]string{"alertname": "crash", "kwerft_rule": "crash", "severity": "critical", "namespace": "alertshop", "app": "web", "pod": "web-1"}
	platformResolved := map[string]string{"alertname": "disk", "kwerft_rule": "disk", "severity": "critical", "node": "n1"}
	vm := fakeMetrics(t,
		[]map[string]any{series(resolvedLabels, now.Add(-time.Hour).Unix()), series(firingLabels, now.Unix()), series(platformResolved, now.Add(-2*time.Hour).Unix())},
		[]map[string]any{series(resolvedLabels, now.Add(-3*time.Hour).Unix())})
	e := newAlertsConsole(t, func(al *alertsAPI) { al.metrics = &alerting.Metrics{URL: vm.URL} })
	e.project(t, "alertshop")
	until := now.Add(time.Hour).UTC().Truncate(time.Second)
	e.am.mu.Lock()
	e.am.silences["s1"] = alerting.Silence{ID: "s1", EndsAt: until, Matchers: []alerting.Matcher{{Name: "namespace", Value: "alertshop"}}}
	e.am.mu.Unlock()
	e.am.set(
		amAlert(firingLabels, "alertshop/web is crash-looping"),
		amAlert(map[string]string{"kwerft_rule": "mem", "severity": "critical", "node": "n1"}, "Node n1 is low on memory"),
		amAlert(map[string]string{"kwerft_rule": "restarts", "severity": "warning", "namespace": "alertshop", "app": "api"}, "api restarts", "s1"),
		amAlert(map[string]string{"kwerft_rule": "crash", "severity": "critical", "namespace": "kube-system", "pod": "coredns-1"}, "kube-system/coredns-1 is crash-looping"),
	)

	all := alertsOf(t, e.owner, "")
	if got := rulesOfAlerts(all); !slices.Equal(got, []string{"crash/firing", "crash/firing", "mem/firing", "restarts/silenced"}) {
		t.Errorf("owner sees %v", got)
	}
	if all[0].Severity != "critical" {
		t.Errorf("critical alerts first: %+v", all[0])
	}
	for _, s := range []*session{e.dev, e.viewer} {
		got := alertsOf(t, s, "")
		if r := rulesOfAlerts(got); !slices.Equal(r, []string{"crash/firing", "restarts/silenced"}) {
			t.Errorf("developer/viewer sees %v", r)
		}
		for _, a := range got {
			if a.Project != "alertshop" || a.App == "" {
				t.Errorf("alert %+v", a)
			}
		}
	}
	silenced := alertsOf(t, e.dev, "?state=silenced")
	if len(silenced) != 1 || silenced[0].SilencedUntil == nil || !silenced[0].SilencedUntil.Equal(until) || !slices.Equal(silenced[0].SilencedBy, []string{"s1"}) {
		t.Errorf("silenced = %+v", silenced)
	}
	firing := alertsOf(t, e.owner, "?state=firing")
	if len(firing) != 3 || firing[0].ConsoleURL == "" || firing[0].Summary == "" || firing[0].Fingerprint == "" {
		t.Errorf("firing = %+v", firing)
	}
	// Only Kwerft's alerts are asked for, not the stack's own rules.
	e.am.mu.Lock()
	if f := e.am.filters[0]; f != `kwerft_rule=~".+"` {
		t.Errorf("filter = %q", f)
	}
	e.am.mu.Unlock()

	// Resolved: from the ALERTS series, minus what still fires, confined.
	resolved := alertsOf(t, e.owner, "?state=resolved")
	if r := rulesOfAlerts(resolved); !slices.Equal(r, []string{"disk/resolved", "restarts/resolved"}) {
		t.Fatalf("owner resolved = %v", r)
	}
	dev := alertsOf(t, e.dev, "?state=resolved")
	if len(dev) != 1 || dev[0].Rule != "restarts" || dev[0].Project != "alertshop" || dev[0].App != "web" ||
		dev[0].EndsAt == nil || dev[0].EndsAt.Unix() != now.Add(-time.Hour).Unix() || dev[0].StartsAt.Unix() != now.Add(-3*time.Hour).Unix() ||
		dev[0].ConsoleURL != "https://console.example.com/apps/alertshop/web?tab=logs" || dev[0].Labels["alertstate"] != "" ||
		dev[0].Fingerprint != alerting.Fingerprint(resolvedLabels) {
		t.Errorf("developer resolved = %+v", dev)
	}

	var bad apiError
	if code := e.owner.do(t, "GET", "/api/v1/alerts?state=gone", nil, &bad); code != http.StatusBadRequest {
		t.Errorf("bad state: %d", code)
	}
	e.am.mu.Lock()
	e.am.down = true
	e.am.mu.Unlock()
	if code := e.owner.do(t, "GET", "/api/v1/alerts", nil, &bad); code != http.StatusServiceUnavailable || !strings.Contains(bad.Error, "Alertmanager") {
		t.Errorf("Alertmanager down: %d %+v", code, bad)
	}
}

func TestSilencesAreConfinedAndAudited(t *testing.T) {
	e := newAlertsConsole(t)
	e.project(t, "silenceshop")
	shop := amAlert(map[string]string{"kwerft_rule": "crash", "severity": "critical", "namespace": "silenceshop", "app": "web"}, "web crashes")
	node := amAlert(map[string]string{"kwerft_rule": "mem", "severity": "critical", "node": "n1"}, "n1 memory")
	e.am.set(shop, node)
	shopFP, nodeFP := alerting.Fingerprint(shop.Labels), alerting.Fingerprint(node.Labels)

	// A developer silences an alert of a project: every label, exactly.
	var s silenceJSON
	if code := e.dev.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": shopFP, "duration": "1h"}, &s); code != http.StatusCreated {
		t.Fatalf("silence: %d %+v", code, s)
	}
	e.am.mu.Lock()
	stored := e.am.silences[s.ID]
	e.am.mu.Unlock()
	if stored.CreatedBy != "developer@example.com" || stored.Comment != "Silenced from the Kwerft console" || len(stored.Matchers) != 4 ||
		stored.EndsAt.Sub(stored.StartsAt) != time.Hour || s.Project != "silenceshop" {
		t.Errorf("stored silence = %+v, answer %+v", stored, s)
	}
	for _, m := range stored.Matchers {
		if !m.Equal() || m.Value != shop.Labels[m.Name] {
			t.Errorf("matcher %+v", m)
		}
	}

	// Not a platform alert, not one without a project, not as a viewer.
	var bad apiError
	if code := e.dev.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": nodeFP, "duration": "1h"}, &bad); code != http.StatusNotFound {
		t.Errorf("developer silences a node alert: %d", code)
	}
	if code := e.dev.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"matchers": []map[string]any{{"name": "kwerft_rule", "value": "mem"}}, "duration": "1h"}, &bad); code != http.StatusForbidden {
		t.Errorf("developer silences by rule across projects: %d", code)
	}
	if code := e.viewer.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": shopFP, "duration": "1h"}, &bad); code != http.StatusForbidden {
		t.Errorf("viewer silences: %d", code)
	}
	for _, body := range []map[string]any{
		{"fingerprint": shopFP, "duration": "forever"},
		{"fingerprint": shopFP, "duration": "45d"},
		{"fingerprint": shopFP},
	} {
		if code := e.dev.do(t, "POST", "/api/v1/alerts/silences", body, &bad); code != http.StatusBadRequest || bad.Field != "duration" {
			t.Errorf("%v: %d %+v", body, code, bad)
		}
	}
	if code := e.owner.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"matchers": []map[string]any{{"name": "kwerft_rule", "value": ".*", "isRegex": true}}, "duration": "1h"}, &bad); code != http.StatusBadRequest || bad.Field != "matchers" {
		t.Errorf("match-all silence: %d %+v", code, bad)
	}
	// Owners silence platform alerts too.
	var ownerSilence silenceJSON
	if code := e.owner.do(t, "POST", "/api/v1/alerts/silences", map[string]any{"fingerprint": nodeFP, "duration": "24h", "comment": "maintenance"}, &ownerSilence); code != http.StatusCreated {
		t.Errorf("owner silences a node alert: %d", code)
	}

	// Listing and deleting follow the same lines.
	var list []silenceJSON
	e.dev.do(t, "GET", "/api/v1/alerts/silences", nil, &list)
	if len(list) != 1 || list[0].ID != s.ID {
		t.Errorf("developer lists %+v", list)
	}
	e.owner.do(t, "GET", "/api/v1/alerts/silences", nil, &list)
	if len(list) != 2 {
		t.Errorf("owner lists %+v", list)
	}
	if code := e.dev.do(t, "DELETE", "/api/v1/alerts/silences/"+ownerSilence.ID, nil, &bad); code != http.StatusNotFound {
		t.Errorf("developer deletes a platform silence: %d", code)
	}
	if code := e.dev.do(t, "DELETE", "/api/v1/alerts/silences/"+s.ID, nil, nil); code != http.StatusNoContent {
		t.Errorf("developer deletes their project's silence: %d", code)
	}
	if code := e.dev.do(t, "DELETE", "/api/v1/alerts/silences/"+s.ID, nil, nil); code != http.StatusNotFound {
		t.Errorf("deleting twice: %d", code)
	}

	entries, err := e.store.RecentAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Actor+" "+en.Action)
	}
	for _, want := range []string{"developer@example.com alerts.silence_create", "owner@example.com alerts.silence_create",
		"developer@example.com alerts.silence_delete", "developer@example.com alerts.silence_create.denied"} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit lacks %q: %v", want, actions)
		}
	}
}

func deleteRules(t *testing.T, names ...string) {
	t.Cleanup(func() {
		for _, n := range names {
			_ = cluster.admin.Delete(context.Background(), &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
	})
}

func ruleNamed(t *testing.T, s *session, name string) (ruleJSON, bool) {
	t.Helper()
	var list []ruleJSON
	if code := s.do(t, "GET", "/api/v1/alerts/rules", nil, &list); code != http.StatusOK {
		t.Fatalf("list rules: %d", code)
	}
	for _, r := range list {
		if r.Name == name {
			return r, true
		}
	}
	return ruleJSON{}, false
}

func TestAlertRulesAPI(t *testing.T) {
	e := newAlertsConsole(t)
	deleteRules(t, "api-restarts", "api-custom", "api-bad")

	// Developers create rules from built-in conditions; durations both ways
	// as Go durations (days accepted).
	var created ruleJSON
	body := map[string]any{"name": "api-restarts", "condition": "Restarts", "threshold": 3, "window": "1h", "for": "",
		"scope": map[string]any{"projects": []string{"shop"}, "apps": []string{}}, "channels": []string{}}
	if code := e.dev.do(t, "POST", "/api/v1/alerts/rules", body, &created); code != http.StatusCreated {
		t.Fatalf("developer creates a rule: %d %+v", code, created)
	}
	if created.Window != "1h0m0s" || created.Severity != "warning" || created.Description != "More than 3 restarts in 1 hour, in project shop" ||
		!strings.Contains(created.EffectiveExpr, "[1h]") || created.Default {
		t.Errorf("created = %+v", created)
	}
	e.am.set(amAlert(map[string]string{"kwerft_rule": "api-restarts", "severity": "warning", "namespace": "shop", "app": "web"}, "web restarts"))
	eventually(t, func() error {
		r, ok := ruleNamed(t, e.viewer, "api-restarts")
		if !ok || !r.Ready {
			return fmt.Errorf("rule = %+v", r)
		}
		return nil
	})

	// Custom expressions are owners' and admins'.
	custom := map[string]any{"name": "api-custom", "condition": "Custom", "expr": "sum(up) < 1", "severity": "critical"}
	var bad apiError
	if code := e.dev.do(t, "POST", "/api/v1/alerts/rules", custom, &bad); code != http.StatusForbidden || !strings.Contains(bad.Error, "Custom") {
		t.Errorf("developer creates a Custom rule: %d %+v", code, bad)
	}
	if code := e.viewer.do(t, "POST", "/api/v1/alerts/rules", body, &bad); code != http.StatusForbidden {
		t.Errorf("viewer creates a rule: %d", code)
	}
	if code := e.owner.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"name": "api-bad", "condition": "Custom", "expr": "sum(up"}, &bad); code != http.StatusBadRequest || bad.Field != "expr" {
		t.Errorf("invalid expr: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "POST", "/api/v1/alerts/rules", custom, &created); code != http.StatusCreated {
		t.Fatalf("owner creates a Custom rule: %d", code)
	}
	if code := e.dev.do(t, "PUT", "/api/v1/alerts/rules/api-custom", map[string]any{"condition": "Custom", "expr": "up == 0", "disabled": true}, &bad); code != http.StatusForbidden {
		t.Errorf("developer changes a Custom rule: %d", code)
	}
	if code := e.dev.do(t, "PUT", "/api/v1/alerts/rules/api-custom", map[string]any{"condition": "Restarts"}, &bad); code != http.StatusForbidden {
		t.Errorf("developer turns a Custom rule into another: %d", code)
	}
	if code := e.dev.do(t, "DELETE", "/api/v1/alerts/rules/api-custom", nil, &bad); code != http.StatusForbidden {
		t.Errorf("developer deletes a Custom rule: %d", code)
	}
	if code := e.dev.do(t, "PUT", "/api/v1/alerts/rules/api-restarts", map[string]any{"condition": "Custom", "expr": "up"}, &bad); code != http.StatusForbidden {
		t.Errorf("developer turns a rule into a Custom one: %d", code)
	}
	var stored kwerftv1.AlertRule
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: "api-custom"}, &stored); err != nil || stored.Spec.Expr != "sum(up) < 1" || stored.Spec.Disabled {
		t.Errorf("the Custom rule changed: %+v %v", stored.Spec, err)
	}

	// Field errors next to the inputs.
	for _, c := range []struct {
		body  map[string]any
		field string
	}{
		{map[string]any{"name": "Bad Name", "condition": "Restarts"}, "name"},
		{map[string]any{"name": "x", "condition": "Restarts", "window": "soon"}, "window"},
		{map[string]any{"name": "x", "condition": "Restarts", "for": "-5m"}, "for"},
		{map[string]any{"name": "x", "condition": "CrashLooping", "threshold": 2}, "threshold"},
		{map[string]any{"name": "x", "condition": "MemoryHigh", "threshold": 120}, "threshold"},
		{map[string]any{"name": "x", "condition": "Nope"}, "condition"},
		{map[string]any{"name": "x", "condition": "NodeDiskPressure", "scope": map[string]any{"projects": []string{"shop"}}}, "scope"},
	} {
		if code := e.dev.do(t, "POST", "/api/v1/alerts/rules", c.body, &bad); code != http.StatusBadRequest || bad.Field != c.field {
			t.Errorf("%v: %d %+v", c.body, code, bad)
		}
	}

	// Updates, firing counts, delete.
	var updated ruleJSON
	if code := e.dev.do(t, "PUT", "/api/v1/alerts/rules/api-restarts", map[string]any{"condition": "Restarts", "threshold": 10, "for": "2m", "channels": []string{"nowhere"}}, &updated); code != http.StatusOK {
		t.Fatalf("update: %d", code)
	}
	if updated.Threshold == nil || *updated.Threshold != 10 || updated.For != "2m0s" || updated.Window != "" || !slices.Equal(updated.Channels, []string{"nowhere"}) {
		t.Errorf("updated = %+v", updated)
	}
	eventually(t, func() error {
		r, _ := ruleNamed(t, e.owner, "api-restarts")
		if r.Ready || !strings.Contains(r.Message, "nowhere") || r.Firing != 1 {
			return fmt.Errorf("rule = %+v", r)
		}
		return nil
	})
	if code := e.owner.do(t, "PUT", "/api/v1/alerts/rules/api-restarts", map[string]any{"name": "renamed", "condition": "Restarts"}, &bad); code != http.StatusBadRequest || bad.Field != "name" {
		t.Errorf("rename: %d %+v", code, bad)
	}
	if code := e.dev.do(t, "DELETE", "/api/v1/alerts/rules/api-restarts", nil, nil); code != http.StatusNoContent {
		t.Errorf("developer deletes their rule: %d", code)
	}
	if code := e.owner.do(t, "DELETE", "/api/v1/alerts/rules/api-custom", nil, nil); code != http.StatusNoContent {
		t.Errorf("owner deletes a Custom rule: %d", code)
	}

	var conds []alertConditionJSON
	if code := e.viewer.do(t, "GET", "/api/v1/alerts/conditions", nil, &conds); code != http.StatusOK || len(conds) != len(alerting.Catalog) {
		t.Fatalf("conditions: %d %d", code, len(conds))
	}
	for _, c := range conds {
		switch c.Condition {
		case "Restarts":
			if c.Window != "15m0s" || c.Threshold.Default != 5 || c.For != "0s" {
				t.Errorf("Restarts = %+v", c)
			}
		case "Custom":
			if !c.OwnersOnly || c.Scoped {
				t.Errorf("Custom = %+v", c)
			}
		}
	}
}

// The chart's kwerft-controller ClusterRole lets the reconcilers manage the
// operator's objects (the test reconcilers run as admin).
func TestControllerRoleCoversAlerting(t *testing.T) {
	requireCluster(t)
	sys, err := client.New(cluster.console, client.Options{Scheme: cluster.admin.Scheme()})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		resource, verb string
		want           bool
	}{
		{"vmrules", "patch", true}, {"vmalertmanagerconfigs", "patch", true}, {"vmalertmanagerconfigs", "delete", true},
		{"vmalertmanagers", "list", true}, {"vmalertmanagers", "patch", false},
	} {
		review := &authzv1.SelfSubjectAccessReview{Spec: authzv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authzv1.ResourceAttributes{
			Namespace: observability.Namespace, Group: "operator.victoriametrics.com", Resource: c.resource, Verb: c.verb}}}
		if err := sys.Create(context.Background(), review); err != nil {
			t.Fatal(err)
		}
		if review.Status.Allowed != c.want {
			t.Errorf("console may %s %s: %v, want %v", c.verb, c.resource, review.Status.Allowed, c.want)
		}
	}
}

// noSecrets checks responses, the audit log and the console log.
func (e *alertsEnv) noSecrets(t *testing.T, bodies []string, secrets ...string) {
	t.Helper()
	entries, err := e.store.RecentAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	var audit strings.Builder
	for _, en := range entries {
		fmt.Fprintf(&audit, "%s %s %s %s\n", en.Actor, en.Action, en.Target, en.Detail)
	}
	for _, s := range secrets {
		for _, b := range bodies {
			if strings.Contains(b, s) {
				t.Errorf("a response carries a secret: %s", b)
			}
		}
		if strings.Contains(audit.String(), s) {
			t.Errorf("the audit log carries a secret:\n%s", audit.String())
		}
		if strings.Contains(e.logs.String(), s) {
			t.Errorf("the log carries a secret:\n%s", e.logs.String())
		}
	}
}

func storedChannelSecret(t *testing.T, name string) map[string][]byte {
	t.Helper()
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: observability.Namespace, Name: observability.ChannelSecret(name)}, &sec); err != nil {
		t.Fatal(err)
	}
	return sec.Data
}

func TestNotificationChannelsWithWriteOnlySecretsAndTestSend(t *testing.T) {
	var mu sync.Mutex
	var slackBodies []string
	status, answer := http.StatusOK, "ok"
	slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		slackBodies = append(slackBodies, r.URL.Path+" "+string(raw))
		w.WriteHeader(status)
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(slack.Close)
	e := newAlertsConsole(t, func(al *alertsAPI) { al.notifier.HTTP = slack.Client() })
	t.Cleanup(func() {
		for _, n := range []string{"api-ops", "api-mail"} {
			_ = cluster.admin.Delete(context.Background(), &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: n}})
		}
	})
	deleteRules(t, "api-notifies")
	hook := slack.URL + "/services/T000/B000/XXXXsecretXXXX"
	newHook := slack.URL + "/services/T000/B000/YYYYsecretYYYY"
	var bodies []string
	keep := func(r rawResponse) rawResponse { bodies = append(bodies, r.body); return r }

	input := map[string]any{"name": "api-ops", "type": "slack", "slack": map[string]any{"channel": "#ops"}, "url": hook}
	for _, s := range []*session{e.dev, e.viewer} {
		if r := keep(s.raw(t, "POST", "/api/v1/alerts/channels", input)); r.code != http.StatusForbidden {
			t.Errorf("non-admin creates a channel: %d", r.code)
		}
	}
	for _, c := range []struct {
		change map[string]any
		field  string
	}{
		{map[string]any{"name": "Ops!"}, "name"},
		{map[string]any{"url": ""}, "url"},
		{map[string]any{"url": "http://hooks.slack.com/services/x"}, "url"},
		{map[string]any{"slack": map[string]any{"channel": "#Not A Channel"}}, "slack.channel"},
		{map[string]any{"type": "pager"}, "type"},
		{map[string]any{"type": "email", "email": map[string]any{"to": []string{"nope"}, "from": "k@example.com", "smtpHost": "smtp.example.com:587"}}, "email.to"},
		{map[string]any{"type": "email", "email": map[string]any{"to": []string{"a@example.com"}, "from": "k@example.com", "smtpHost": "smtp.example.com"}}, "email.smtpHost"},
		{map[string]any{"type": "email", "email": map[string]any{"to": []string{"a@example.com"}, "from": "k@example.com", "smtpHost": "smtp.example.com:587", "username": "u"}}, "password"},
		{map[string]any{"type": "ntfy", "ntfy": map[string]any{"topic": "a b"}}, "ntfy.topic"},
		{map[string]any{"type": "ntfy", "ntfy": map[string]any{"server": "http://ntfy.example.com", "topic": "t"}}, "ntfy.server"},
	} {
		body := map[string]any{}
		for k, v := range input {
			body[k] = v
		}
		for k, v := range c.change {
			body[k] = v
		}
		var bad apiError
		if code := e.owner.do(t, "POST", "/api/v1/alerts/channels", body, &bad); code != http.StatusBadRequest || bad.Field != c.field {
			t.Errorf("%v: %d %+v", c.change, code, bad)
		}
	}

	r := keep(e.owner.raw(t, "POST", "/api/v1/alerts/channels", input))
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	if got := string(storedChannelSecret(t, "api-ops")[observability.KeyURL]); got != hook {
		t.Errorf("stored url = %q", got)
	}
	eventually(t, func() error {
		var list []channelJSON
		keep(e.viewer.raw(t, "GET", "/api/v1/alerts/channels", nil))
		e.viewer.do(t, "GET", "/api/v1/alerts/channels", nil, &list)
		for _, c := range list {
			if c.Name == "api-ops" && c.Ready && c.SecretSet && c.Slack != nil && c.Slack.Channel == "#ops" && c.SendResolved {
				return nil
			}
		}
		return fmt.Errorf("list = %+v", list)
	})

	// Kubernetes holds the line: owners and admins patch the Secret, nobody reads it.
	for _, check := range []struct {
		role, verb, name string
		want             bool
	}{
		{"owner", "patch", "notify-api-ops", true},
		{"admin", "patch", "notify-api-ops", true},
		{"owner", "get", "notify-api-ops", false},
		{"owner", "list", "", false},
		{"owner", "create", "", false},
		{"owner", "patch", "vmalertmanager-vm-victoria-metrics-k8s-stack", false},
		{"developer", "patch", "notify-api-ops", false},
		{"viewer", "patch", "notify-api-ops", false},
	} {
		if got := canName(t, check.role, observability.Namespace, check.verb, "", "secrets", "", check.name); got != check.want {
			t.Errorf("%s may %s secret %q: %v, want %v", check.role, check.verb, check.name, got, check.want)
		}
	}
	if canName(t, "developer", "", "patch", "kwerft.dev", "notificationchannels", "status", "api-ops") {
		t.Error("developers may write channel status")
	}

	// Test send: straight to Slack, recorded in the status.
	var res struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if code := e.dev.do(t, "POST", "/api/v1/alerts/channels/api-ops/test", nil, &res); code != http.StatusForbidden {
		t.Errorf("developer test-sends: %d", code)
	}
	r = keep(e.owner.raw(t, "POST", "/api/v1/alerts/channels/api-ops/test", nil))
	_ = json.Unmarshal([]byte(r.body), &res)
	mu.Lock()
	if r.code != http.StatusOK || !res.OK || len(slackBodies) != 1 || !strings.Contains(slackBodies[0], "/services/T000/B000/XXXXsecretXXXX") ||
		!strings.Contains(slackBodies[0], `"channel":"#ops"`) || !strings.Contains(slackBodies[0], "[TEST]") {
		t.Errorf("test send: %d %+v %v", r.code, res, slackBodies)
	}
	status, answer = http.StatusForbidden, "invalid_token"
	mu.Unlock()
	r = keep(e.owner.raw(t, "POST", "/api/v1/alerts/channels/api-ops/test", nil))
	_ = json.Unmarshal([]byte(r.body), &res)
	if res.OK || !strings.Contains(res.Message, "Slack answered 403: invalid_token") {
		t.Errorf("failed test send: %+v", res)
	}
	var ch kwerftv1.NotificationChannel
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: "api-ops"}, &ch); err != nil || ch.Status.LastTest == nil ||
		!strings.Contains(ch.Status.LastTestError, "invalid_token") {
		t.Errorf("status after the test: %+v %v", ch.Status, err)
	}
	mu.Lock()
	status, answer = http.StatusOK, "ok"
	mu.Unlock()

	// Edits keep the URL unless a new one is entered; the type stays.
	var view channelJSON
	if code := e.owner.do(t, "PUT", "/api/v1/alerts/channels/api-ops", map[string]any{"type": "slack", "slack": map[string]any{"channel": "#alerts"}, "sendResolved": false}, &view); code != http.StatusOK ||
		view.Slack.Channel != "#alerts" || view.SendResolved {
		t.Errorf("update: %d %+v", code, view)
	}
	if got := string(storedChannelSecret(t, "api-ops")[observability.KeyURL]); got != hook {
		t.Errorf("the URL changed without a new one: %q", got)
	}
	r = keep(e.owner.raw(t, "PUT", "/api/v1/alerts/channels/api-ops", map[string]any{"type": "slack", "url": newHook}))
	if r.code != http.StatusOK || string(storedChannelSecret(t, "api-ops")[observability.KeyURL]) != newHook {
		t.Errorf("new URL: %d", r.code)
	}
	var bad apiError
	if code := e.owner.do(t, "PUT", "/api/v1/alerts/channels/api-ops", map[string]any{"type": "webhook", "url": newHook}, &bad); code != http.StatusBadRequest || bad.Field != "type" {
		t.Errorf("type change: %d %+v", code, bad)
	}

	// An email channel: the password is write-only too.
	r = keep(e.owner.raw(t, "POST", "/api/v1/alerts/channels", map[string]any{"name": "api-mail", "type": "email", "password": "pa55-word-secret",
		"email": map[string]any{"to": []string{"ops@example.com"}, "from": "kwerft@example.com", "smtpHost": "smtp.example.com:587", "username": "kwerft"}}))
	if r.code != http.StatusCreated || string(storedChannelSecret(t, "api-mail")[observability.KeyPassword]) != "pa55-word-secret" {
		t.Errorf("email create: %d %s", r.code, r.body)
	}

	// A rule naming the channel keeps it from being deleted.
	if code := e.owner.do(t, "POST", "/api/v1/alerts/rules", map[string]any{"name": "api-notifies", "condition": "CrashLooping", "channels": []string{"api-ops"}}, nil); code != http.StatusCreated {
		t.Fatalf("rule: %d", code)
	}
	eventually(t, func() error {
		var list []channelJSON
		e.owner.do(t, "GET", "/api/v1/alerts/channels", nil, &list)
		for _, c := range list {
			if c.Name == "api-ops" && slices.Equal(c.Rules, []string{"api-notifies"}) && strings.Contains(c.Message, "api-notifies") {
				return nil
			}
		}
		return fmt.Errorf("list = %+v", list)
	})
	if code := e.owner.do(t, "DELETE", "/api/v1/alerts/channels/api-ops", nil, &bad); code != http.StatusConflict || !strings.Contains(bad.Error, "api-notifies") {
		t.Errorf("delete while used: %d %+v", code, bad)
	}
	if code := e.owner.do(t, "DELETE", "/api/v1/alerts/rules/api-notifies", nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete rule: %d", code)
	}
	if code := e.owner.do(t, "DELETE", "/api/v1/alerts/channels/api-ops", nil, nil); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
	eventually(t, func() error {
		err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: observability.Namespace, Name: "notify-api-ops"}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("secret still there: %v", err)
		}
		return nil
	})

	e.noSecrets(t, bodies, "XXXXsecretXXXX", "YYYYsecretYYYY", "pa55-word-secret")
}
