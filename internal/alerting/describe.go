package alerting

import (
	"errors"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Describe explains a valid rule in one plain sentence, e.g. "More than 5
// restarts in 15 minutes, in all projects".
func Describe(spec *kwerftv1.AlertRuleSpec) string {
	e := Resolve(spec)
	t := itoa(e.Threshold)
	forPart := ""
	if e.For > 0 {
		forPart = " for " + Humanize(e.For)
	}
	var s string
	switch e.Condition {
	case kwerftv1.AlertCrashLooping:
		s = "A container keeps crashing (CrashLoopBackOff)" + forPart
	case kwerftv1.AlertRestarts:
		s = "More than " + t + " restarts in " + Humanize(e.Window) + forPart
	case kwerftv1.AlertMemoryHigh:
		s = "Memory above " + t + " % of the limit" + forPart
	case kwerftv1.AlertCPUHigh:
		s = "CPU above " + t + " % of the limit" + forPart
	case kwerftv1.AlertVolumeFillingUp:
		s = "A volume is more than " + t + " % full, or full within " + Humanize(e.Window) + " at the current rate" + forPart
	case kwerftv1.AlertNodeMemoryPressure:
		s = "Less than " + t + " % of a node's memory available" + forPart
	case kwerftv1.AlertNodeDiskPressure:
		s = "Less than " + t + " % of a node's disk free" + forPart
	case kwerftv1.AlertCertificateExpiring:
		s = "A certificate expires within " + Humanize(e.Window)
	case kwerftv1.AlertScheduleFailing:
		s = "A schedule's last run failed"
		if e.Window > 0 {
			s += ", or it has not succeeded within " + Humanize(e.Window)
		}
		s += forPart
	case kwerftv1.AlertBuildFailing:
		s = "An app's latest build failed" + forPart
	case kwerftv1.AlertHTTPErrorRate:
		s = "More than " + t + " % of requests fail with 5xx over " + Humanize(e.Window) + forPart
	case kwerftv1.AlertHTTPLatency:
		s = "95th percentile latency above " + t + " ms over " + Humanize(e.Window) + forPart
	case kwerftv1.AlertBackupFailing:
		s = "A backup plan's latest backup failed" + forPart
	case kwerftv1.AlertBackupMissing:
		if e.Window > 0 {
			s = "A backup plan has not completed a backup within " + Humanize(e.Window) + forPart
		} else {
			s = "A backup plan has not completed a backup within twice its interval" + forPart
		}
	case kwerftv1.AlertUpgradeFailed:
		s = "The latest upgrade of Kwerft or Kubernetes failed or was rolled back (fires for " + Humanize(e.Window) + ")" + forPart
	case kwerftv1.AlertCustom:
		s = "Custom expression" + forPart
	}
	switch {
	case !e.Scoped():
	case len(spec.Scope.Projects) > 0 && len(spec.Scope.Apps) > 0:
		s += ", in " + plural(len(spec.Scope.Projects), "project ", "projects ") + strings.Join(spec.Scope.Projects, ", ") +
			" and " + plural(len(spec.Scope.Apps), "app ", "apps ") + strings.Join(spec.Scope.Apps, ", ")
	case len(spec.Scope.Projects) > 0:
		s += ", in " + plural(len(spec.Scope.Projects), "project ", "projects ") + strings.Join(spec.Scope.Projects, ", ")
	case len(spec.Scope.Apps) > 0:
		s += ", for " + plural(len(spec.Scope.Apps), "app ", "apps ") + strings.Join(spec.Scope.Apps, ", ")
	case e.Kind == KindCertificate:
		s += ", for the console and every domain"
	default:
		s += ", in all projects"
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Humanize writes a duration in words: "15 minutes", "1 hour 30 minutes", "7 days".
func Humanize(d time.Duration) string {
	if d <= 0 {
		return "0 seconds"
	}
	units := []struct {
		d          time.Duration
		one, other string
	}{{day, "day", "days"}, {time.Hour, "hour", "hours"}, {time.Minute, "minute", "minutes"}, {time.Second, "second", "seconds"}}
	var parts []string
	for _, u := range units {
		if n := d / u.d; n > 0 {
			name := u.other
			if n == 1 {
				name = u.one
			}
			parts = append(parts, strconv.FormatInt(int64(n), 10)+" "+name)
			d -= n * u.d
		}
	}
	return strings.Join(parts, " ")
}

// FormatDuration is the compact form the console API uses: "15m", "1h30m",
// "7d", "0s".
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	var b strings.Builder
	for _, u := range []struct {
		d time.Duration
		s string
	}{{day, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.d; n > 0 {
			b.WriteString(strconv.FormatInt(int64(n), 10) + u.s)
			d -= n * u.d
		}
	}
	return b.String()
}

// ParseDuration reads a Go duration with an optional leading days part:
// "7d", "1d12h", "15m", "90s".
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("empty duration")
	}
	var days time.Duration
	if i := strings.IndexByte(s, 'd'); i >= 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil || n < 0 || n > 3650 {
			return 0, errors.New("invalid duration")
		}
		days = time.Duration(n) * day
		s = s[i+1:]
		if s == "" {
			return days, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, errors.New("invalid duration")
	}
	return days + d, nil
}

// ---- default rules ---------------------------------------------------------------

// DefaultRule is one of the rules Kwerft creates (label kwerft.dev/default).
type DefaultRule struct {
	Name string
	Spec kwerftv1.AlertRuleSpec
}

// DefaultRules are created when missing and never changed afterwards: a
// user's edits stay, and deleting one restores it with these values. None
// notifies anyone until a channel is added.
func DefaultRules() []DefaultRule {
	rule := func(name string, c kwerftv1.AlertCondition) DefaultRule {
		info, _ := Lookup(c)
		return DefaultRule{Name: name, Spec: kwerftv1.AlertRuleSpec{Condition: c, Severity: info.Severity}}
	}
	return []DefaultRule{
		rule("crash-looping", kwerftv1.AlertCrashLooping),
		rule("restarts", kwerftv1.AlertRestarts),
		rule("memory-high", kwerftv1.AlertMemoryHigh),
		rule("volume-filling-up", kwerftv1.AlertVolumeFillingUp),
		rule("node-memory-pressure", kwerftv1.AlertNodeMemoryPressure),
		rule("node-disk-pressure", kwerftv1.AlertNodeDiskPressure),
		rule("certificate-expiring", kwerftv1.AlertCertificateExpiring),
		rule("schedule-failing", kwerftv1.AlertScheduleFailing),
		rule("build-failing", kwerftv1.AlertBuildFailing),
		rule("backup-failing", kwerftv1.AlertBackupFailing),
		rule("backup-missing", kwerftv1.AlertBackupMissing),
		rule("upgrade-failed", kwerftv1.AlertUpgradeFailed),
	}
}

// IsDefault reports whether a rule is one Kwerft created.
func IsDefault(r *kwerftv1.AlertRule) bool { return r.Labels[observability.LabelDefault] == "true" }

// Duration wraps d for a spec field.
func Duration(d time.Duration) *metav1.Duration { return &metav1.Duration{Duration: d} }
