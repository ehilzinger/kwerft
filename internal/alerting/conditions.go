// Package alerting turns AlertRules into vmalert expressions, explains them
// in plain language, talks to Alertmanager's API and sends test
// notifications. The reconcilers (internal/controllers/alerting_*.go) and the
// console API (internal/server/api_alerts.go) share it, so the console
// validates a rule exactly as the reconciler renders it.
package alerting

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/VictoriaMetrics/metricsql"
	"k8s.io/apimachinery/pkg/util/validation"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Kind is what a condition watches; it decides which scope applies, which
// labels the alerts carry and where their console link points.
type Kind string

const (
	KindApp         Kind = "app"         // pods and HTTP traffic of Apps: namespace, app
	KindVolume      Kind = "volume"      // PersistentVolumeClaims: namespace, persistentvolumeclaim
	KindNode        Kind = "node"        // nodes; platform alerts, no scope
	KindCertificate Kind = "certificate" // Domain and console certificates
	KindSchedule    Kind = "schedule"    // Schedules: namespace, schedule
	KindCustom      Kind = "custom"      // spec.expr; no scope
)

// Unit of a threshold.
const (
	UnitCount   = "count"
	UnitPercent = "percent"
	UnitMillis  = "ms"
)

// Threshold describes a condition's threshold.
type Threshold struct {
	Default int64  `json:"default"`
	Unit    string `json:"unit"`
	Min     int64  `json:"min"`
	Max     int64  `json:"max"`
}

// Info is what Kwerft knows about one condition: its defaults and what it
// accepts. Window is nil when the condition has no window; DefaultWindow nil
// with HasWindow means the window is optional and off by default.
type Info struct {
	Condition     kwerftv1.AlertCondition `json:"condition"`
	Label         string                  `json:"label"`
	Kind          Kind                    `json:"kind"`
	Threshold     *Threshold              `json:"threshold,omitempty"`
	HasWindow     bool                    `json:"hasWindow"`
	DefaultWindow time.Duration           `json:"-"`
	DefaultFor    time.Duration           `json:"-"`
	Severity      kwerftv1.AlertSeverity  `json:"severity"`
	// Interval overrides vmalert's evaluation interval (20s) for the rule's
	// group; CrashLooping runs every 10s for the 2-minute exit criterion.
	Interval time.Duration `json:"-"`
}

// Scoped reports whether rules of this condition accept a scope.
func (i Info) Scoped() bool { return i.Kind != KindNode && i.Kind != KindCustom }

const day = 24 * time.Hour

// Catalog lists every condition in the order the console offers them.
var Catalog = []Info{
	{Condition: kwerftv1.AlertCrashLooping, Label: "Crash looping", Kind: KindApp, Severity: "critical", Interval: 10 * time.Second},
	{Condition: kwerftv1.AlertRestarts, Label: "Restarts", Kind: KindApp, Severity: "warning",
		Threshold: &Threshold{Default: 5, Unit: UnitCount, Min: 1, Max: 10000}, HasWindow: true, DefaultWindow: 15 * time.Minute},
	{Condition: kwerftv1.AlertMemoryHigh, Label: "Memory near the limit", Kind: KindApp, Severity: "warning",
		Threshold: &Threshold{Default: 90, Unit: UnitPercent, Min: 1, Max: 100}, DefaultFor: 10 * time.Minute},
	{Condition: kwerftv1.AlertCPUHigh, Label: "CPU near the limit", Kind: KindApp, Severity: "warning",
		Threshold: &Threshold{Default: 90, Unit: UnitPercent, Min: 1, Max: 100}, DefaultFor: 15 * time.Minute},
	{Condition: kwerftv1.AlertVolumeFillingUp, Label: "Volume filling up", Kind: KindVolume, Severity: "warning",
		Threshold: &Threshold{Default: 85, Unit: UnitPercent, Min: 1, Max: 100}, HasWindow: true, DefaultWindow: 7 * day, DefaultFor: 10 * time.Minute},
	{Condition: kwerftv1.AlertNodeMemoryPressure, Label: "Node memory low", Kind: KindNode, Severity: "critical",
		Threshold: &Threshold{Default: 10, Unit: UnitPercent, Min: 1, Max: 99}, DefaultFor: 10 * time.Minute},
	{Condition: kwerftv1.AlertNodeDiskPressure, Label: "Node disk low", Kind: KindNode, Severity: "critical",
		Threshold: &Threshold{Default: 10, Unit: UnitPercent, Min: 1, Max: 99}, DefaultFor: 5 * time.Minute},
	{Condition: kwerftv1.AlertCertificateExpiring, Label: "Certificate expiring", Kind: KindCertificate, Severity: "warning",
		HasWindow: true, DefaultWindow: 14 * day, DefaultFor: 10 * time.Minute},
	{Condition: kwerftv1.AlertScheduleFailing, Label: "Schedule failing", Kind: KindSchedule, Severity: "warning", HasWindow: true},
	{Condition: kwerftv1.AlertBuildFailing, Label: "Build failing", Kind: KindApp, Severity: "warning"},
	{Condition: kwerftv1.AlertHTTPErrorRate, Label: "HTTP errors", Kind: KindApp, Severity: "warning",
		Threshold: &Threshold{Default: 5, Unit: UnitPercent, Min: 1, Max: 100}, HasWindow: true, DefaultWindow: 5 * time.Minute, DefaultFor: 5 * time.Minute},
	{Condition: kwerftv1.AlertHTTPLatency, Label: "HTTP latency", Kind: KindApp, Severity: "warning",
		Threshold: &Threshold{Default: 1000, Unit: UnitMillis, Min: 1, Max: 600000}, HasWindow: true, DefaultWindow: 5 * time.Minute, DefaultFor: 10 * time.Minute},
	{Condition: kwerftv1.AlertCustom, Label: "Custom expression", Kind: KindCustom, Severity: "warning"},
}

// Lookup returns the catalog entry of a condition.
func Lookup(c kwerftv1.AlertCondition) (Info, bool) {
	for _, i := range Catalog {
		if i.Condition == c {
			return i, true
		}
	}
	return Info{}, false
}

// Limits on windows and "for".
const (
	MinWindow = time.Minute
	MaxWindow = 90 * day
	MaxFor    = day
)

// Effective holds a rule's values with the defaults filled in.
type Effective struct {
	Info
	Threshold int64
	Window    time.Duration // 0: none
	For       time.Duration
}

// Resolve fills in a spec's defaults. The spec must be valid (Validate).
func Resolve(spec *kwerftv1.AlertRuleSpec) Effective {
	info, _ := Lookup(spec.Condition)
	e := Effective{Info: info, Window: info.DefaultWindow, For: info.DefaultFor}
	if info.Threshold != nil {
		e.Threshold = info.Threshold.Default
		if spec.Threshold != nil {
			e.Threshold = *spec.Threshold
		}
	}
	if spec.Window != nil && info.HasWindow {
		e.Window = spec.Window.Duration
	}
	if spec.For != nil {
		e.For = spec.For.Duration
	}
	return e
}

// FieldError is a validation problem with the field it concerns, in the
// console API's field names.
type FieldError struct {
	Field, Message string
}

func (e *FieldError) Error() string { return e.Field + ": " + e.Message }

func fieldErr(field, format string, args ...any) *FieldError {
	return &FieldError{Field: field, Message: fmt.Sprintf(format, args...)}
}

// Validate checks a spec beyond what the CRD schema can: thresholds, windows
// and scope per condition, and Custom expressions (MetricsQL). The
// reconciler runs it too, so a rule written past the console is reported
// instead of breaking the shared VMRule.
func Validate(spec *kwerftv1.AlertRuleSpec) *FieldError {
	info, ok := Lookup(spec.Condition)
	if !ok {
		return fieldErr("condition", "Choose one of the conditions Kwerft knows.")
	}
	switch spec.Severity {
	case "", "critical", "warning", "info":
	default:
		return fieldErr("severity", "Choose critical, warning or info.")
	}
	if spec.Threshold != nil {
		t := info.Threshold
		if t == nil {
			return fieldErr("threshold", "%s has no threshold.", info.Label)
		}
		if *spec.Threshold < t.Min || *spec.Threshold > t.Max {
			return fieldErr("threshold", "Enter a number from %d to %d.", t.Min, t.Max)
		}
	}
	if spec.Window != nil {
		if !info.HasWindow {
			return fieldErr("window", "%s has no window.", info.Label)
		}
		if w := spec.Window.Duration; w < MinWindow || w > MaxWindow {
			return fieldErr("window", "Enter a window from 1 minute to 90 days.")
		}
	}
	if spec.For != nil && (spec.For.Duration < 0 || spec.For.Duration > MaxFor) {
		return fieldErr("for", "Enter a duration from 0 to 24 hours.")
	}
	if info.Kind == KindCustom {
		if strings.TrimSpace(spec.Expr) == "" {
			return fieldErr("expr", "Enter a MetricsQL expression.")
		}
		if len(spec.Expr) > 4096 {
			return fieldErr("expr", "The expression is too long (at most 4096 characters).")
		}
		if err := ValidateExpr(spec.Expr); err != nil {
			return fieldErr("expr", "%s", err.Error())
		}
	} else if spec.Expr != "" {
		return fieldErr("expr", "Only Custom rules have an expression.")
	}
	if !info.Scoped() && (len(spec.Scope.Projects) > 0 || len(spec.Scope.Apps) > 0) {
		return fieldErr("scope", "%s applies to the whole cluster and takes no projects or apps.", info.Label)
	}
	for _, p := range spec.Scope.Projects {
		if len(validation.IsDNS1123Label(p)) > 0 {
			return fieldErr("scope.projects", "%q is not a project name.", p)
		}
	}
	for _, a := range spec.Scope.Apps {
		p, n, ok := strings.Cut(a, "/")
		if !ok || len(validation.IsDNS1123Label(p)) > 0 || len(validation.IsDNS1123Label(n)) > 0 {
			return fieldErr("scope.apps", "%q is not an app; write it as project/app.", a)
		}
	}
	for _, c := range spec.Channels {
		if len(validation.IsDNS1123Label(c)) > 0 {
			return fieldErr("channels", "%q is not a channel name.", c)
		}
	}
	return nil
}

// ValidateExpr parses a MetricsQL expression as vmalert would.
func ValidateExpr(expr string) error {
	parsed, err := metricsql.Parse(expr)
	if err != nil {
		return fmt.Errorf("not a valid MetricsQL expression: %w", err)
	}
	if _, ok := parsed.(*metricsql.StringExpr); ok {
		return fmt.Errorf("the expression must return series, not a string")
	}
	return nil
}

// ---- rendering --------------------------------------------------------------------

// Rendered is one vmalert alerting rule.
type Rendered struct {
	Alert       string
	Expr        string
	For         time.Duration
	Interval    time.Duration // 0: vmalert's default
	Labels      map[string]string
	Annotations map[string]string
}

// Render turns a rule into its vmalert form. consoleHost (may be empty)
// makes the console_url annotation.
func Render(rule *kwerftv1.AlertRule, consoleHost string) (Rendered, error) {
	if ferr := Validate(&rule.Spec); ferr != nil {
		return Rendered{}, ferr
	}
	e := Resolve(&rule.Spec)
	severity := string(rule.Spec.Severity)
	if severity == "" {
		severity = "warning"
	}
	out := Rendered{
		Alert:    rule.Name,
		Expr:     Expr(&rule.Spec),
		For:      e.For,
		Interval: e.Interval,
		Labels:   map[string]string{observability.LabelRule: rule.Name, "severity": severity},
	}
	summary, description := annotations(e, rule.Name)
	out.Annotations = map[string]string{"summary": summary, "description": description}
	if consoleHost != "" {
		out.Annotations["console_url"] = consoleURL(e, consoleHost)
	}
	return out, nil
}

// Expr is the MetricsQL expression of a valid spec.
func Expr(spec *kwerftv1.AlertRuleSpec) string {
	e := Resolve(spec)
	s := spec.Scope
	switch e.Condition {
	case kwerftv1.AlertCrashLooping:
		return podExpr(s, "max_over_time(%s[5m])", "kube_pod_container_status_waiting_reason", `reason="CrashLoopBackOff"`) + " >= 1"
	case kwerftv1.AlertRestarts:
		return podExpr(s, "increase(%s["+promDuration(e.Window)+"])", "kube_pod_container_status_restarts_total") + " > " + itoa(e.Threshold)
	case kwerftv1.AlertMemoryHigh:
		return usageExpr(s, "kwerft:container_memory_working_set_bytes", "memory", e.Threshold)
	case kwerftv1.AlertCPUHigh:
		return usageExpr(s, "kwerft:container_cpu_usage_cores:rate5m", "cpu", e.Threshold)
	case kwerftv1.AlertVolumeFillingUp:
		f := scopeBranches(s, "")
		by := "max by (namespace, persistentvolumeclaim) "
		pct := "(100 * " + by + "(" + selector("kubelet_volume_stats_used_bytes", nil, f) + ") / " +
			by + "(" + selector("kubelet_volume_stats_capacity_bytes", nil, f) + "))"
		predict := by + "(predict_linear(" + selector("kubelet_volume_stats_available_bytes", nil, f) + "[6h], " +
			itoa(int64(e.Window/time.Second)) + "))"
		return pct + " > " + itoa(e.Threshold) + " or (" + pct + " > 50 and " + predict + " < 0)"
	case kwerftv1.AlertNodeMemoryPressure:
		return nodeExpr(`100 * node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes`) + " < " + itoa(e.Threshold)
	case kwerftv1.AlertNodeDiskPressure:
		fs := `fstype=~"ext[234]|xfs|btrfs|zfs",mountpoint!~"/boot.*"`
		return nodeExpr(`100 * node_filesystem_avail_bytes{`+fs+`} / node_filesystem_size_bytes{`+fs+`}`, "mountpoint") + " < " + itoa(e.Threshold)
	case kwerftv1.AlertCertificateExpiring:
		w := itoa(int64(e.Window / time.Second))
		out := "(min by (namespace, domain, hostname) (" + selector("kwerft_domain_certificate_expiry_timestamp_seconds", nil, scopeBranches(s, "")) + ") - time()) < " + w
		if len(s.Projects) == 0 && len(s.Apps) == 0 {
			// The console's own certificates are the platform's; only an
			// unscoped rule watches them.
			out += " or (min by (hostname, purpose) (kwerft_console_certificate_expiry_timestamp_seconds) - time()) < " + w
		}
		return out
	case kwerftv1.AlertScheduleFailing:
		f := scopeBranches(s, "")
		by := "max by (namespace, schedule) "
		failure := by + "(" + selector("kwerft_schedule_last_failure_timestamp_seconds", nil, f) + ")"
		success := by + "(" + selector("kwerft_schedule_last_success_timestamp_seconds", nil, f) + ")"
		// A failure newer than the last success; a schedule that never
		// succeeded compares against 0.
		out := failure + " > on (namespace, schedule) (" + success + " or " + failure + " * 0)"
		if e.Window > 0 {
			out += " or (time() - " + success + ") > " + itoa(int64(e.Window/time.Second))
		}
		return out
	case kwerftv1.AlertBuildFailing:
		return "max by (namespace, app) (" + selector("kwerft_app_latest_build_failed", nil, scopeBranches(s, "app")) + ") == 1"
	case kwerftv1.AlertHTTPErrorRate:
		f := scopeBranches(s, "app")
		errs := overWindow(selector("kwerft:http_requests:rate5m", []string{`code_class="5xx"`}, f), e.Window)
		all := overWindow(selector("kwerft:http_requests:rate5m", nil, f), e.Window)
		return "100 * sum by (namespace, app) (" + errs + ") / sum by (namespace, app) (" + all + ") > " + itoa(e.Threshold)
	case kwerftv1.AlertHTTPLatency:
		lat := overWindow(selector("kwerft:http_latency_p95_seconds:5m", nil, scopeBranches(s, "app")), e.Window)
		return "max by (namespace, app) (" + lat + ") * 1000 > " + itoa(e.Threshold)
	case kwerftv1.AlertCustom:
		return strings.TrimSpace(spec.Expr)
	}
	return ""
}

// podExpr is a per-container expression (namespace, pod, container) with the
// app label joined from kube-state-metrics' pod labels (kwerft.dev/app). inner
// has one %s for the selector of metric (base filters plus namespace scope).
// Unscoped or project-scoped rules also keep pods without an app label (they
// carry namespace and pod only); app-scoped rules need the label.
func podExpr(s kwerftv1.AlertScope, inner, metric string, base ...string) string {
	filters := append([]string{}, base...)
	if ns := scopeNamespaces(s); len(ns) > 0 {
		filters = append(filters, `namespace=~"`+strings.Join(ns, "|")+`"`)
	}
	perPod := "max by (namespace, pod, container) (" + fmt.Sprintf(inner, selector(metric, filters, nil)) + ")"
	info := "max by (namespace, pod, app) (label_replace(" +
		selector("kube_pod_labels", []string{`label_kwerft_dev_app!=""`}, scopeBranches(s, "label_kwerft_dev_app")) +
		`, "app", "$1", "label_kwerft_dev_app", "(.+)"))`
	joined := perPod + " * on (namespace, pod) group_left (app) " + info
	if len(s.Apps) > 0 {
		return "(" + joined + ")"
	}
	return "(" + joined + " or on (namespace, pod, container) " + perPod + ")"
}

// nodeExpr gives a node-exporter expression the label node: the node of the
// exporter's pod (kube-state-metrics' kube_pod_info), or else the address
// from instance. The exporter pod's own namespace and pod labels are dropped,
// so node alerts are platform alerts.
func nodeExpr(inner string, extra ...string) string {
	keep := strings.Join(append([]string{"instance"}, extra...), ", ")
	m := "max by (" + keep + ", namespace, pod) (" + inner + ")"
	joined := "max by (node, " + keep + ") (" + m + ` * on (namespace, pod) group_left (node) max by (namespace, pod, node) (kube_pod_info{node!=""}))`
	fallback := "max by (node, " + keep + `) (label_replace(` + m + `, "node", "$1", "instance", "(.+?)(:[0-9]+)?"))`
	return "(" + joined + " or on (" + keep + ") " + fallback + ")"
}

// usageExpr: an app's usage as percent of its pods' limits.
func usageExpr(s kwerftv1.AlertScope, metric, resource string, threshold int64) string {
	usage := "max by (namespace, app, pod) (" + selector(metric, nil, scopeBranches(s, "app")) + ")"
	limitFilters := []string{`resource="` + resource + `"`}
	if ns := scopeNamespaces(s); len(ns) > 0 {
		limitFilters = append(limitFilters, `namespace=~"`+strings.Join(ns, "|")+`"`)
	}
	limits := "sum by (namespace, pod) (" + selector("kube_pod_container_resource_limits", limitFilters, nil) + ")"
	return "100 * " + usage + " / on (namespace, pod) group_left () " + limits + " > " + itoa(threshold)
}

func overWindow(sel string, w time.Duration) string {
	if w == 0 || w == 5*time.Minute {
		return sel
	}
	return "avg_over_time(" + sel + "[" + promDuration(w) + "])"
}

// scopeBranches turns a scope into MetricsQL "or" filter groups: one for the
// projects, one per app. appLabel names the label holding the app ("" when
// the metric has none; apps then narrow to their project).
func scopeBranches(s kwerftv1.AlertScope, appLabel string) [][]string {
	var out [][]string
	if len(s.Projects) > 0 {
		out = append(out, []string{`namespace=~"` + strings.Join(sortedUnique(s.Projects), "|") + `"`})
	}
	seen := map[string]bool{}
	for _, a := range sortedUnique(s.Apps) {
		p, n, _ := strings.Cut(a, "/")
		branch := []string{`namespace="` + p + `"`}
		if appLabel != "" {
			branch = append(branch, appLabel+`="`+n+`"`)
		}
		key := strings.Join(branch, ",")
		if !seen[key] && !(appLabel == "" && slices.Contains(s.Projects, p)) {
			seen[key] = true
			out = append(out, branch)
		}
	}
	return out
}

// scopeNamespaces are all namespaces a scope touches (nil: all).
func scopeNamespaces(s kwerftv1.AlertScope) []string {
	if len(s.Projects) == 0 && len(s.Apps) == 0 {
		return nil
	}
	ns := append([]string{}, s.Projects...)
	for _, a := range s.Apps {
		p, _, _ := strings.Cut(a, "/")
		ns = append(ns, p)
	}
	return sortedUnique(ns)
}

// selector renders metric{base…} or, with branches, metric{base…,b1… or base…,b2…}.
func selector(metric string, base []string, branches [][]string) string {
	if len(branches) == 0 {
		if len(base) == 0 {
			return metric
		}
		return metric + "{" + strings.Join(base, ",") + "}"
	}
	groups := make([]string, 0, len(branches))
	for _, b := range branches {
		groups = append(groups, strings.Join(append(append([]string{}, base...), b...), ","))
	}
	return metric + "{" + strings.Join(groups, " or ") + "}"
}

func sortedUnique(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// promDuration formats a duration for a range selector, e.g. 15m, 7d, 90s.
func promDuration(d time.Duration) string {
	switch {
	case d%day == 0:
		return itoa(int64(d/day)) + "d"
	case d%time.Hour == 0:
		return itoa(int64(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return itoa(int64(d/time.Minute)) + "m"
	}
	return itoa(int64(d/time.Second)) + "s"
}

// ---- annotations and links --------------------------------------------------------------

// Templates vmalert expands per alert ($labels, $value).
const (
	appTarget  = `{{ $labels.namespace }}/{{ or $labels.app $labels.pod }}`
	nodeTarget = `{{ or $labels.node $labels.instance }}`
	pctValue   = `{{ printf "%.0f" $value }} %`
)

func annotations(e Effective, rule string) (summary, description string) {
	t := itoa(e.Threshold)
	switch e.Condition {
	case kwerftv1.AlertCrashLooping:
		return appTarget + ` is crash-looping`,
			`Container {{ $labels.container }} of pod {{ $labels.pod }} keeps crashing; Kubernetes waits longer before each restart (CrashLoopBackOff). Its logs usually say why.`
	case kwerftv1.AlertRestarts:
		return appTarget + ` restarted {{ printf "%.0f" $value }} times`,
			`Container {{ $labels.container }} of pod {{ $labels.pod }} restarted {{ printf "%.0f" $value }} times in ` + Humanize(e.Window) + ` (alert above ` + t + `).`
	case kwerftv1.AlertMemoryHigh:
		return appTarget + ` uses ` + pctValue + ` of its memory limit`,
			`Pod {{ $labels.pod }} has used more than ` + t + ` % of its memory limit for ` + Humanize(e.For) + `. At the limit Kubernetes kills it (OOMKilled).`
	case kwerftv1.AlertCPUHigh:
		return appTarget + ` uses ` + pctValue + ` of its CPU limit`,
			`Pod {{ $labels.pod }} has used more than ` + t + ` % of its CPU limit for ` + Humanize(e.For) + `; it is being throttled.`
	case kwerftv1.AlertVolumeFillingUp:
		return `Volume {{ $labels.namespace }}/{{ $labels.persistentvolumeclaim }} is ` + pctValue + ` full`,
			`The volume is more than ` + t + ` % full, or fills up within ` + Humanize(e.Window) + ` at the rate of the last 6 hours.`
	case kwerftv1.AlertNodeMemoryPressure:
		return `Node ` + nodeTarget + ` has ` + pctValue + ` of its memory available`,
			`Less than ` + t + ` % of the node's memory has been available for ` + Humanize(e.For) + `. Pods may be evicted or killed.`
	case kwerftv1.AlertNodeDiskPressure:
		return `Node ` + nodeTarget + ` has ` + pctValue + ` disk space free on {{ $labels.mountpoint }}`,
			`Less than ` + t + ` % of {{ $labels.mountpoint }} is free. Kubernetes evicts pods and stops pulling images when the disk fills up.`
	case kwerftv1.AlertCertificateExpiring:
		return `The certificate for {{ $labels.hostname }} expires in {{ $value | humanizeDuration }}`,
			`The TLS certificate for {{ $labels.hostname }} expires within ` + Humanize(e.Window) + ` and has not been renewed. cert-manager renews 30 days ahead; check the Domain's status.`
	case kwerftv1.AlertScheduleFailing:
		d := `The last run of the schedule failed.`
		if e.Window > 0 {
			d = `The last run of the schedule failed, or it has not succeeded within ` + Humanize(e.Window) + `.`
		}
		return `Schedule {{ $labels.namespace }}/{{ $labels.schedule }} is failing`, d + ` The run's logs say why.`
	case kwerftv1.AlertBuildFailing:
		return `The latest build of {{ $labels.namespace }}/{{ $labels.app }} failed`,
			`The app keeps running its previous revision until a build succeeds. The build log says why.`
	case kwerftv1.AlertHTTPErrorRate:
		return `{{ $labels.namespace }}/{{ $labels.app }} answers {{ printf "%.1f" $value }} % of requests with 5xx`,
			`More than ` + t + ` % of requests failed with a server error over ` + Humanize(e.Window) + `.`
	case kwerftv1.AlertHTTPLatency:
		return `{{ $labels.namespace }}/{{ $labels.app }} is slow: p95 {{ printf "%.0f" $value }} ms`,
			`95 % of requests took less than {{ printf "%.0f" $value }} ms over ` + Humanize(e.Window) + `, above the ` + t + ` ms set.`
	}
	return `Alert ` + rule + ` is firing`,
		`Value {{ $value }} for{{ range $k, $v := $labels }} {{ $k }}={{ $v }}{{ end }}`
}

// consoleURL is the console page an alert links to.
func consoleURL(e Effective, host string) string {
	base := "https://" + host
	switch e.Kind {
	case KindApp:
		tab := "logs"
		if e.Condition == kwerftv1.AlertBuildFailing {
			tab = "builds"
		}
		// Pods without an app label (platform pods) have no app page.
		return `{{ if $labels.app }}` + base + `/apps/{{ $labels.namespace }}/{{ $labels.app }}?tab=` + tab +
			`{{ else }}` + base + `/monitoring{{ end }}`
	case KindVolume:
		return base + `/apps/volumes?project={{ $labels.namespace }}`
	case KindSchedule:
		return base + `/jobs/{{ $labels.namespace }}/schedules/{{ $labels.schedule }}`
	case KindCertificate:
		return `{{ if $labels.domain }}` + base + `/network{{ else }}` + base + `/settings{{ end }}`
	case KindNode:
		return base + "/monitoring/metrics"
	}
	return `{{ if and $labels.namespace $labels.app }}` + base + `/apps/{{ $labels.namespace }}/{{ $labels.app }}?tab=logs{{ else }}` + base + `/monitoring{{ end }}`
}
