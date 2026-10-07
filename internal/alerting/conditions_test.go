// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package alerting

import (
	"slices"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/VictoriaMetrics/metricsql"
	"github.com/prometheus/common/model"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

// Backup alerts are platform alerts on Kwerft's plan metrics: no
// namespace survives the aggregation, and paused plans (no interval) never
// count as missing.
func TestBackupExpressions(t *testing.T) {
	failing := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBackupFailing})
	if failing != "max by (plan) (kwerft_backup_plan_last_backup_failed) == 1" {
		t.Errorf("failing: %s", failing)
	}
	missing := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBackupMissing})
	if !strings.Contains(missing, "> on (plan) 2 * max by (plan) (kwerft_backup_plan_interval_seconds)") ||
		!strings.Contains(missing, "kwerft_backup_plan_last_success_timestamp_seconds or kwerft_backup_plan_created_timestamp_seconds") {
		t.Errorf("missing: %s", missing)
	}
	windowed := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBackupMissing, Window: Duration(3 * day)})
	if !strings.Contains(windowed, "> 259200 and on (plan) max by (plan) (kwerft_backup_plan_interval_seconds)") {
		t.Errorf("windowed: %s", windowed)
	}
	if ferr := Validate(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBackupFailing, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}}); ferr == nil || ferr.Field != "scope" {
		t.Errorf("a backup rule took a scope: %v", ferr)
	}
	if got := Describe(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBackupMissing}); got != "A backup plan has not completed a backup within twice its interval" {
		t.Errorf("describe: %s", got)
	}
}

// Upgrade alerts are platform alerts on Kwerft's own metric, for a day
// after the failure unless the rule says otherwise, and link to Settings ›
// Updates.
func TestUpgradeFailedExpression(t *testing.T) {
	got := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertUpgradeFailed})
	if got != "time() - max by (component, version, upgrade, result) (kwerft_upgrade_failed_timestamp_seconds) < 86400" {
		t.Errorf("expr: %s", got)
	}
	if err := ValidateExpr(got); err != nil {
		t.Error(err)
	}
	if got := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertUpgradeFailed, Window: Duration(time.Hour)}); !strings.HasSuffix(got, " < 3600") {
		t.Errorf("windowed: %s", got)
	}
	if ferr := Validate(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertUpgradeFailed, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}}); ferr == nil || ferr.Field != "scope" {
		t.Errorf("an upgrade rule took a scope: %v", ferr)
	}
	r, err := Render(&kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "upgrade-failed"}, Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertUpgradeFailed}}, "ops.example.com")
	if err != nil || r.Annotations["console_url"] != "https://ops.example.com/settings/updates" || !strings.Contains(r.Annotations["summary"], "rolled back") {
		t.Errorf("render: %+v %v", r, err)
	}
	if !slices.ContainsFunc(DefaultRules(), func(d DefaultRule) bool { return d.Name == "upgrade-failed" && d.Spec.Severity == "critical" }) {
		t.Error("no default rule upgrade-failed")
	}
}

// vmalertFuncs stands in for vmalert's template functions, enough to parse
// and execute the annotations Kwerft renders.
var vmalertFuncs = template.FuncMap{
	"humanize":           func(v float64) string { return "h" },
	"humanizeDuration":   func(v float64) string { return "d" },
	"humanizePercentage": func(v float64) string { return "p" },
	"toUpper":            strings.ToUpper,
}

func expand(t *testing.T, text string, labels map[string]string) string {
	t.Helper()
	tmpl, err := template.New("a").Funcs(vmalertFuncs).Parse("{{ $labels := .Labels }}{{ $value := .Value }}" + text)
	if err != nil {
		t.Fatalf("template %q: %v", text, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, map[string]any{"Labels": labels, "Value": 42.0}); err != nil {
		t.Fatalf("execute %q: %v", text, err)
	}
	return b.String()
}

func scopes() []kwerftv1.AlertScope {
	return []kwerftv1.AlertScope{
		{},
		{Projects: []string{"shop", "api"}},
		{Apps: []string{"shop/web", "api/worker"}},
		{Projects: []string{"shop"}, Apps: []string{"api/worker", "shop/web"}},
	}
}

// Every condition, with defaults and with every kind of scope, renders a
// MetricsQL expression that parses, and annotations that expand.
func TestEveryConditionRendersValidMetricsQL(t *testing.T) {
	for _, info := range Catalog {
		ss := scopes()
		if !info.Scoped() {
			ss = ss[:1]
		}
		for _, s := range ss {
			rule := &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "r"}, Spec: kwerftv1.AlertRuleSpec{Condition: info.Condition, Scope: s}}
			if info.Kind == KindCustom {
				rule.Spec.Expr = `up == 0`
			}
			out, err := Render(rule, "console.example.com")
			if err != nil {
				t.Fatalf("%s %+v: %v", info.Condition, s, err)
			}
			if _, err := metricsql.Parse(out.Expr); err != nil {
				t.Errorf("%s %+v: %q does not parse: %v", info.Condition, s, out.Expr, err)
			}
			if out.Labels["kwerft_rule"] != "r" || out.Labels["kwerft_kind"] != string(info.Kind) || out.Labels["severity"] != "warning" {
				t.Errorf("%s labels = %v", info.Condition, out.Labels)
			}
			labels := map[string]string{"namespace": "shop", "app": "web", "pod": "web-1", "container": "web", "node": "n1",
				"persistentvolumeclaim": "data", "schedule": "nightly", "hostname": "shop.example.com", "domain": "shop", "mountpoint": "/",
				"device": "nvme0", "plan": "cluster", "component": "Kwerft", "version": "0.6.0", "upgrade": "kwerft-0.6.0-x7k2p", "result": "RolledBack"}
			for k, v := range out.Annotations {
				if got := expand(t, v, labels); got == "" || strings.Contains(got, "<no value>") {
					t.Errorf("%s annotation %s expands to %q", info.Condition, k, got)
				}
			}
			if s.Projects == nil && s.Apps == nil && !strings.HasPrefix(out.Annotations["console_url"], "https://console.example.com/") {
				// The certificate link is a template choosing between two pages.
				if got := expand(t, out.Annotations["console_url"], labels); !strings.HasPrefix(got, "https://console.example.com/") {
					t.Errorf("%s console_url = %q", info.Condition, got)
				}
			}
		}
	}
}

func TestCrashLoopingExpression(t *testing.T) {
	expr := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping})
	for _, want := range []string{
		`max_over_time(kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"}[5m])`,
		`group_left (app)`,
		`label_replace(kube_pod_labels{label_kwerft_dev_app!=""}, "app", "$1", "label_kwerft_dev_app", "(.+)")`,
		// Pods without the app label still alert (namespace and pod only).
		`or on (namespace, pod, container)`,
		`>= 1`,
		// Early crash loops: two restarts within 3 minutes, before
		// Kubernetes settles on reporting CrashLoopBackOff.
		`increase(kube_pod_container_status_restarts_total[3m])`,
		`>= 2`,
	} {
		if !strings.Contains(expr, want) {
			t.Errorf("expression lacks %q:\n%s", want, expr)
		}
	}
	// Scoped to apps: only those apps, nothing without an app label.
	scoped := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping, Scope: kwerftv1.AlertScope{Apps: []string{"shop/web"}}})
	if !strings.Contains(scoped, `kube_pod_labels{label_kwerft_dev_app!="",namespace="shop",label_kwerft_dev_app="web"}`) ||
		strings.Contains(scoped, "or on") || !strings.Contains(scoped, `namespace=~"shop"`) {
		t.Errorf("app-scoped expression:\n%s", scoped)
	}
	// Scoped to projects: a namespace filter, pods without app label kept.
	proj := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping, Scope: kwerftv1.AlertScope{Projects: []string{"shop", "api"}}})
	if !strings.Contains(proj, `reason="CrashLoopBackOff",namespace=~"api|shop"`) || !strings.Contains(proj, "or on") {
		t.Errorf("project-scoped expression:\n%s", proj)
	}
	r, _ := Render(&kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "crash"}, Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping}}, "c.example.com")
	if r.For != 0 || r.Interval != 10*time.Second {
		t.Errorf("crash looping fires at once, evaluated every 10s: for %v, interval %v", r.For, r.Interval)
	}
	if got := expand(t, r.Annotations["console_url"], map[string]string{"namespace": "shop", "app": "web"}); got != "https://c.example.com/apps/shop/web?tab=logs" {
		t.Errorf("console_url = %q", got)
	}
	if got := expand(t, r.Annotations["console_url"], map[string]string{"namespace": "kube-system", "pod": "coredns-1"}); got != "https://c.example.com/monitoring" {
		t.Errorf("console_url without app = %q", got)
	}
	if got := expand(t, r.Annotations["summary"], map[string]string{"namespace": "kube-system", "pod": "coredns-1"}); got != "kube-system/coredns-1 is crash-looping" {
		t.Errorf("summary without app = %q", got)
	}
}

func TestThresholdsWindowsAndDefaults(t *testing.T) {
	for _, c := range []struct {
		spec kwerftv1.AlertRuleSpec
		want []string
		desc string
	}{
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts},
			[]string{`increase(kube_pod_container_status_restarts_total[15m])`, `> 5`},
			"More than 5 restarts in 15 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Threshold: ptr(int64(3)), Window: Duration(time.Hour), For: Duration(2 * time.Minute)},
			[]string{`[1h]`, `> 3`},
			"More than 3 restarts in 1 hour for 2 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertMemoryHigh, Scope: kwerftv1.AlertScope{Apps: []string{"shop/web"}}},
			[]string{`kwerft:container_memory_working_set_bytes{namespace="shop",app="web"}`, `kube_pod_container_resource_limits{resource="memory",namespace=~"shop"}`, `> 90`},
			"Memory above 90 % of the limit for 10 minutes, for app shop/web"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCPUHigh, Threshold: ptr(int64(80))},
			[]string{`kwerft:container_cpu_usage_cores:rate5m`, `resource="cpu"`, `> 80`},
			"CPU above 80 % of the limit for 15 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertVolumeFillingUp, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}},
			[]string{`kubelet_volume_stats_used_bytes{namespace=~"shop"}`, `> 85 or`, `predict_linear(kubelet_volume_stats_available_bytes{namespace=~"shop"}[6h], 604800)) < 0`},
			"A volume is more than 85 % full, or full within 7 days at the current rate for 10 minutes, in project shop"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertNodeMemoryPressure},
			[]string{`(100 * node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`, `group_left (node)`,
				`label_replace(`, `"node", "$1", "instance", "(.+?)(:[0-9]+)?"`, `) < 10`},
			"Less than 10 % of a node's memory available for 10 minutes"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertNodeDiskPressure, Threshold: ptr(int64(5))},
			[]string{`node_filesystem_avail_bytes{fstype=~`, `max by (node, instance, mountpoint)`, `or on (instance, mountpoint)`, `< 5`},
			"Less than 5 % of a node's disk free for 5 minutes"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCertificateExpiring},
			[]string{`kwerft_domain_certificate_expiry_timestamp_seconds) - time()) < 1209600`, `kwerft_console_certificate_expiry_timestamp_seconds`},
			"A certificate expires within 14 days, for the console and every domain"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCertificateExpiring, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}, Window: Duration(3 * day)},
			[]string{`kwerft_domain_certificate_expiry_timestamp_seconds{namespace=~"shop"}`, `< 259200`},
			"A certificate expires within 3 days, in project shop"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertScheduleFailing},
			[]string{`kwerft_schedule_last_failure_timestamp_seconds) > on (namespace, schedule)`, `kwerft_schedule_last_success_timestamp_seconds`},
			"A schedule's last run failed, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertScheduleFailing, Window: Duration(day), Scope: kwerftv1.AlertScope{Apps: []string{"shop/web"}}},
			[]string{`(time() - max by (namespace, schedule) (kwerft_schedule_last_success_timestamp_seconds{namespace="shop"})) > 86400`},
			"A schedule's last run failed, or it has not succeeded within 1 day, for app shop/web"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertBuildFailing},
			[]string{`max by (namespace, app) (kwerft_app_latest_build_failed) == 1`},
			"An app's latest build failed, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertHTTPErrorRate},
			[]string{`sum by (namespace, app) (kwerft:http_requests:rate5m{code_class="5xx"})`, `> 5`},
			"More than 5 % of requests fail with 5xx over 5 minutes for 5 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertHTTPErrorRate, Window: Duration(time.Hour)},
			[]string{`avg_over_time(kwerft:http_requests:rate5m{code_class="5xx"}[1h])`},
			"More than 5 % of requests fail with 5xx over 1 hour for 5 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertHTTPLatency, Threshold: ptr(int64(250))},
			[]string{`max by (namespace, app) (kwerft:http_latency_p95_seconds:5m) * 1000 > 250`},
			"95th percentile latency above 250 ms over 5 minutes for 10 minutes, in all projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom, Expr: "  sum(up) < 3 "},
			[]string{`sum(up) < 3`},
			"Custom expression"},
	} {
		if ferr := Validate(&c.spec); ferr != nil {
			t.Errorf("%s: %v", c.spec.Condition, ferr)
			continue
		}
		expr := Expr(&c.spec)
		for _, w := range c.want {
			if !strings.Contains(expr, w) {
				t.Errorf("%s: expression lacks %q:\n%s", c.spec.Condition, w, expr)
			}
		}
		if _, err := metricsql.Parse(expr); err != nil {
			t.Errorf("%s: %v", c.spec.Condition, err)
		}
		if got := Describe(&c.spec); got != c.desc {
			t.Errorf("%s: Describe = %q, want %q", c.spec.Condition, got, c.desc)
		}
	}
}

func TestValidateRejectsWhatTheConditionDoesNotTake(t *testing.T) {
	for _, c := range []struct {
		spec  kwerftv1.AlertRuleSpec
		field string
	}{
		{kwerftv1.AlertRuleSpec{Condition: "Nope"}, "condition"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCrashLooping, Threshold: ptr(int64(2))}, "threshold"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertMemoryHigh, Threshold: ptr(int64(101))}, "threshold"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Threshold: ptr(int64(0))}, "threshold"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertMemoryHigh, Window: Duration(time.Hour)}, "window"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Window: Duration(time.Second)}, "window"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Window: Duration(100 * day)}, "window"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, For: Duration(-time.Second)}, "for"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, For: Duration(2 * day)}, "for"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom}, "expr"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom, Expr: "sum(up"}, "expr"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom, Expr: `"just a string"`}, "expr"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Expr: "up"}, "expr"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertNodeDiskPressure, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}}, "scope"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertCustom, Expr: "up", Scope: kwerftv1.AlertScope{Apps: []string{"shop/web"}}}, "scope"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Scope: kwerftv1.AlertScope{Projects: []string{`shop"} or vector(1)`}}}, "scope.projects"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Scope: kwerftv1.AlertScope{Apps: []string{"web"}}}, "scope.apps"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Scope: kwerftv1.AlertScope{Apps: []string{`shop/w"eb`}}}, "scope.apps"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Channels: []string{"Ops Team"}}, "channels"},
		{kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRestarts, Severity: "panic"}, "severity"},
	} {
		ferr := Validate(&c.spec)
		if ferr == nil || ferr.Field != c.field {
			t.Errorf("%+v: %v, want an error on %s", c.spec, ferr, c.field)
		}
	}
}

func TestDefaultRulesAreValid(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range DefaultRules() {
		if seen[d.Name] {
			t.Errorf("duplicate default %s", d.Name)
		}
		seen[d.Name] = true
		if ferr := Validate(&d.Spec); ferr != nil {
			t.Errorf("%s: %v", d.Name, ferr)
		}
		if len(d.Spec.Channels) > 0 {
			t.Errorf("%s notifies before an owner adds a channel", d.Name)
		}
	}
	if !seen["crash-looping"] || !seen["backup-missing"] || !seen["upgrade-failed"] || !seen["disk-failing"] || len(seen) != 16 {
		t.Errorf("defaults = %v", seen)
	}
}

func TestDurations(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * day, "1d12h": 36 * time.Hour, "15m": 15 * time.Minute, "90s": 90 * time.Second, "0s": 0} {
		got, err := ParseDuration(in)
		if err != nil || got != want {
			t.Errorf("ParseDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "x", "-1m", "d", "5dd"} {
		if _, err := ParseDuration(bad); err == nil {
			t.Errorf("ParseDuration(%q) accepted", bad)
		}
	}
	for d, want := range map[time.Duration]string{0: "0s", 90 * time.Minute: "1h30m", 7 * day: "7d", 36 * time.Hour: "1d12h"} {
		if got := FormatDuration(d); got != want {
			t.Errorf("FormatDuration(%v) = %q", d, got)
		}
	}
	if got := Humanize(90 * time.Minute); got != "1 hour 30 minutes" {
		t.Errorf("Humanize = %q", got)
	}
}

func TestFingerprintMatchesAlertmanager(t *testing.T) {
	labels := map[string]string{"alertname": "crash-looping", "kwerft_rule": "crash-looping", "namespace": "shop", "app": "web", "severity": "critical"}
	ls := model.LabelSet{}
	for k, v := range labels {
		ls[model.LabelName(k)] = model.LabelValue(v)
	}
	if got, want := Fingerprint(labels), ls.Fingerprint().String(); got != want {
		t.Errorf("Fingerprint = %s, Alertmanager's = %s", got, want)
	}
	if got, want := Fingerprint(nil), (model.LabelSet{}).Fingerprint().String(); got != want {
		t.Errorf("empty: %s, want %s", got, want)
	}
}

// Disk alerts are platform alerts per node and disk: RAID from
// node-exporter's md collector, SMART from the chart's smartctl_exporter
// (job kwerft-disk-health), with the drive's model and serial number for a
// replacement request. They link to the node list.
func TestDiskExpressions(t *testing.T) {
	raid := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertRAIDDegraded})
	for _, want := range []string{`node_md_disks_required - ignoring (state) node_md_disks{state="active"}`, `node_md_disks{state="failed"} > 0`,
		`node_md_state{state="inactive"} == 1`, "max by (node, instance, device)"} {
		if !strings.Contains(raid, want) {
			t.Errorf("raid: no %q in %s", want, raid)
		}
	}
	failing := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskFailing})
	for _, want := range []string{`max by (node, device) (smartctl_device_smart_status{job="kwerft-disk-health"}) == 0`,
		`(smartctl_device_critical_warning{job="kwerft-disk-health"}) > 0`, `(smartctl_device_media_errors{job="kwerft-disk-health"}) > min_over_time((max by (node, device) (smartctl_device_media_errors{job="kwerft-disk-health"}))[1d:5m])`,
		`group_left (model_name, serial_number)`} {
		if !strings.Contains(failing, want) {
			t.Errorf("failing: no %q in %s", want, failing)
		}
	}
	if got := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskFailing, Window: Duration(time.Hour)}); !strings.Contains(got, "[1h:1m]") {
		t.Errorf("windowed: %s", got)
	}
	wear := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskWearing})
	if !strings.Contains(wear, `(smartctl_device_percentage_used{job="kwerft-disk-health"}) > 80`) {
		t.Errorf("wear: %s", wear)
	}
	if got := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskWearing, Threshold: ptr[int64](95)}); !strings.Contains(got, "> 95") {
		t.Errorf("wear 95: %s", got)
	}
	missing := Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskReadingsMissing})
	for _, want := range []string{`kube_node_labels{label_kwerft_dev_platform="dedicated"}`, `unless on (node) max by (node) (smartctl_devices{job="kwerft-disk-health"}) > 0`,
		`up{job="node-exporter"}`, `smartctl_device_smartctl_exit_status{job="kwerft-disk-health"} % 8) > 0`} {
		if !strings.Contains(missing, want) {
			t.Errorf("missing: no %q in %s", want, missing)
		}
	}
	for _, c := range []kwerftv1.AlertCondition{kwerftv1.AlertRAIDDegraded, kwerftv1.AlertDiskFailing, kwerftv1.AlertDiskWearing, kwerftv1.AlertDiskReadingsMissing} {
		if ferr := Validate(&kwerftv1.AlertRuleSpec{Condition: c, Scope: kwerftv1.AlertScope{Projects: []string{"shop"}}}); ferr == nil || ferr.Field != "scope" {
			t.Errorf("%s took a scope: %v", c, ferr)
		}
		r, err := Render(&kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: kwerftv1.AlertRuleSpec{Condition: c}}, "ops.example.com")
		if err != nil || r.Annotations["console_url"] != "https://ops.example.com/clusters/local/nodes" {
			t.Errorf("%s render: %+v %v", c, r, err)
		}
	}
	r, _ := Render(&kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskFailing}}, "")
	labels := map[string]string{"node": "dedi-1", "device": "nvme0", "model_name": "SAMSUNG MZVL2512HCJQ-00B00", "serial_number": "S64"}
	if got := expand(t, r.Annotations["summary"], labels); got != "Disk nvme0 on node dedi-1 is failing" {
		t.Errorf("summary: %s", got)
	}
	if got := expand(t, r.Annotations["description"], labels); !strings.Contains(got, "SAMSUNG MZVL2512HCJQ-00B00, serial number S64.") {
		t.Errorf("description: %s", got)
	}
	r, _ = Render(&kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: "d"}, Spec: kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskReadingsMissing}}, "")
	if got := expand(t, r.Annotations["summary"], map[string]string{"node": "dedi-1"}); got != "Node dedi-1 reports no disk health" {
		t.Errorf("missing summary: %s", got)
	}
	if got := Describe(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskWearing}); got != "A disk has used more than 80 % of its rated endurance for 10 minutes" {
		t.Errorf("describe: %s", got)
	}
}
