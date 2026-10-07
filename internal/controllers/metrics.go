// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	toolscache "k8s.io/client-go/tools/cache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
	"github.com/ehilzinger/kwerft/internal/version"
)

// Kwerft's own metrics, served with controller-runtime's on the manager's
// metrics endpoint (:8081, inside the cluster only) and scraped by vmagent
// through the chart's VMServiceScrape. docs/phase3.md lists the names; the
// alert rules (internal/alerting) and the recording rules
// (charts/kwerft/templates/metrics-rules.yaml) rely on them.
//
// Gauges are read from the informer cache at scrape time, so they always
// agree with the objects' status; only the build duration histogram is
// observed as builds finish.

var (
	descScheduleSuccess = prometheus.NewDesc("kwerft_schedule_last_success_timestamp_seconds",
		"When a run of the Schedule last succeeded (status.lastSuccessTime).", []string{"namespace", "schedule"}, nil)
	descScheduleFailure = prometheus.NewDesc("kwerft_schedule_last_failure_timestamp_seconds",
		"When a run of the Schedule last failed (status.lastFailureTime).", []string{"namespace", "schedule"}, nil)
	descBuildFailed = prometheus.NewDesc("kwerft_app_latest_build_failed",
		"1 when the App's latest finished build (pull-request checks aside) failed, 0 when it succeeded.", []string{"namespace", "app"}, nil)
	descDomainExpiry = prometheus.NewDesc("kwerft_domain_certificate_expiry_timestamp_seconds",
		"When the certificate serving the Domain expires (status.notAfter).", []string{"namespace", "domain", "hostname"}, nil)
	descConsoleExpiry = prometheus.NewDesc("kwerft_console_certificate_expiry_timestamp_seconds",
		"When a certificate the console depends on expires, per hostname it covers.", []string{"hostname", "purpose"}, nil)
	descRouteInfo = prometheus.NewDesc("kwerft_http_route_info",
		"Maps Traefik's router names (route label: httproute-<namespace>-<HTTPRoute>) to the App an HTTPRoute serves; value 1.",
		[]string{"namespace", "app", "route"}, nil)
	descBackupSuccess = prometheus.NewDesc("kwerft_backup_plan_last_success_timestamp_seconds",
		"When a backup of the BackupPlan last completed (status.lastSuccessfulAt).", []string{"plan"}, nil)
	descBackupFailed = prometheus.NewDesc("kwerft_backup_plan_last_backup_failed",
		"1 when the BackupPlan's latest backup failed (Failed, PartiallyFailed, FailedValidation), 0 otherwise.", []string{"plan"}, nil)
	descBackupInterval = prometheus.NewDesc("kwerft_backup_plan_interval_seconds",
		"Time between two scheduled runs of a BackupPlan that is not paused.", []string{"plan"}, nil)
	descBackupCreated = prometheus.NewDesc("kwerft_backup_plan_created_timestamp_seconds",
		"When the BackupPlan was created (the age of a plan without a successful backup).", []string{"plan"}, nil)
	descUpgradeFailed = prometheus.NewDesc("kwerft_upgrade_failed_timestamp_seconds",
		"When the newest finished Upgrade of a component (cancelled ones aside) failed or was rolled back (status.finishedAt); no series once a later one succeeded.",
		[]string{"component", "version", "upgrade", "result"}, nil)
	descScrapeError = prometheus.NewDesc("kwerft_metrics_collect_errors",
		"Kinds that could not be read for this scrape (missing CRD, cache not synced).", []string{"kind"}, nil)
)

// buildDuration is observed when a Build reaches Succeeded or Failed.
var buildDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "kwerft_build_duration_seconds",
	Help:    "How long builds ran, from start to completion, by result (succeeded, failed).",
	Buckets: []float64{15, 30, 60, 90, 120, 180, 300, 600, 900, 1800, 3600},
}, []string{"namespace", "app", "result"})

// RegisterMetrics adds Kwerft's metrics to controller-runtime's registry
// (served by the manager's metrics server) and starts observing builds.
func RegisterMetrics(mgr ctrl.Manager) error {
	c := &MetricsCollector{Reader: mgr.GetCache(), Version: version.Version}
	if err := ctrlmetrics.Registry.Register(c); err != nil {
		return err
	}
	if err := ctrlmetrics.Registry.Register(buildDuration); err != nil {
		return err
	}
	return mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		return watchBuildDurations(ctx, mgr.GetCache())
	}))
}

// MetricsCollector reads Schedules, BackupPlans, Upgrades, Builds, Domains,
// ConsoleSettings and HTTPRoutes at scrape time.
type MetricsCollector struct {
	Reader client.Reader
	// Version is the running Kwerft's; "" leaves a failed Kwerft Upgrade
	// standing although the version arrived another way.
	Version string
	// Timeout bounds one scrape's reads; 0 means 10s.
	Timeout time.Duration
}

func (c *MetricsCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descScheduleSuccess, descScheduleFailure, descBuildFailed,
		descDomainExpiry, descConsoleExpiry, descRouteInfo, descBackupSuccess, descBackupFailed, descBackupInterval,
		descBackupCreated, descUpgradeFailed, descScrapeError} {
		ch <- d
	}
}

func (c *MetricsCollector) Collect(ch chan<- prometheus.Metric) {
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	failed := func(kind string) {
		ch <- prometheus.MustNewConstMetric(descScrapeError, prometheus.GaugeValue, 1, kind)
	}
	gauge := func(d *prometheus.Desc, t time.Time, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, float64(t.Unix()), labels...)
	}

	var schedules kwerftv1.ScheduleList
	if err := c.Reader.List(ctx, &schedules); err != nil {
		failed("Schedule")
	}
	for _, s := range schedules.Items {
		if t := s.Status.LastSuccessTime; t != nil {
			gauge(descScheduleSuccess, t.Time, s.Namespace, s.Name)
		}
		if t := s.Status.LastFailureTime; t != nil {
			gauge(descScheduleFailure, t.Time, s.Namespace, s.Name)
		}
	}

	var plans kwerftv1.BackupPlanList
	if err := c.Reader.List(ctx, &plans); err != nil {
		failed("BackupPlan")
	}
	for _, p := range plans.Items {
		gauge(descBackupCreated, p.CreationTimestamp.Time, p.Name)
		if t := p.Status.LastSuccessfulAt; t != nil {
			gauge(descBackupSuccess, t.Time, p.Name)
		}
		v := 0.0
		if last := p.Status.LastBackup; last != nil && BackupFailed(last.Phase) {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(descBackupFailed, prometheus.GaugeValue, v, p.Name)
		if sched, err := ParseBackupSchedule(p.Spec.Schedule); err == nil && !p.Spec.Paused {
			ch <- prometheus.MustNewConstMetric(descBackupInterval, prometheus.GaugeValue, BackupInterval(sched, time.Now()).Seconds(), p.Name)
		}
	}

	var ups kwerftv1.UpgradeList
	if err := c.Reader.List(ctx, &ups); err != nil {
		failed("Upgrade")
	}
	var nodes corev1.NodeList
	if err := c.Reader.List(ctx, &nodes); err != nil {
		failed("Node")
	}
	for _, u := range FailedUpgrades(ups.Items, runningVersions(c.Version, nodes.Items)) {
		gauge(descUpgradeFailed, u.Status.FinishedAt.Time, string(u.Spec.Component), u.Spec.Version, u.Name, string(u.Status.Phase))
	}

	var bl kwerftv1.BuildList
	if err := c.Reader.List(ctx, &bl); err != nil {
		failed("Build")
	}
	for key, failedBuild := range latestBuildResults(bl.Items) {
		v := 0.0
		if failedBuild {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(descBuildFailed, prometheus.GaugeValue, v, key.Namespace, key.Name)
	}

	var domains kwerftv1.DomainList
	if err := c.Reader.List(ctx, &domains); err != nil {
		failed("Domain")
	}
	for _, d := range domains.Items {
		if d.Status.NotAfter != nil {
			gauge(descDomainExpiry, d.Status.NotAfter.Time, d.Namespace, d.Name, d.Spec.Hostname)
		}
	}

	var settings kwerftv1.ConsoleSettings
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &settings); client.IgnoreNotFound(err) != nil {
		failed("ConsoleSettings")
	}
	seen := map[[2]string]bool{}
	for _, cert := range settings.Status.Certificates {
		if cert.NotAfter == nil {
			continue
		}
		for _, h := range cert.Hostnames {
			if k := [2]string{h, cert.Purpose}; !seen[k] {
				seen[k] = true
				gauge(descConsoleExpiry, cert.NotAfter.Time, h, cert.Purpose)
			}
		}
	}

	var routes gwv1.HTTPRouteList
	if err := c.Reader.List(ctx, &routes, client.MatchingLabels{LabelManagedBy: ManagedByKwerft}); err != nil {
		failed("HTTPRoute")
	}
	for _, r := range routes.Items {
		app := r.Labels[LabelApp]
		if app == "" || !hasBackend(&r) {
			continue // redirects (HTTP → HTTPS) serve nothing of the App's
		}
		ch <- prometheus.MustNewConstMetric(descRouteInfo, prometheus.GaugeValue, 1, r.Namespace, app, TraefikRoute(r.Namespace, r.Name))
	}
}

// TraefikRoute is the start of the names Traefik's Gateway API provider
// gives the routers of an HTTPRoute (Traefik v3.7 makeRouterName:
// "httproute-<namespace>-<name>-gw-…", normalised to letters, digits and
// dashes, which DNS names already are).
func TraefikRoute(namespace, name string) string {
	return "httproute-" + namespace + "-" + name
}

func hasBackend(r *gwv1.HTTPRoute) bool {
	for _, rule := range r.Spec.Rules {
		if len(rule.BackendRefs) > 0 {
			return true
		}
	}
	return false
}

// FailedUpgrades are the newest finished Upgrade of each component when it
// failed or was rolled back (the alert UpgradeFailed). Cancelled Upgrades
// changed nothing and are passed over; a later success clears the
// component, and so does running its version or a newer one already
// (install.sh run by hand after the Upgrade failed), per running.
func FailedUpgrades(items []kwerftv1.Upgrade, running map[kwerftv1.UpgradeComponent]upgrades.Version) []kwerftv1.Upgrade {
	latest := map[kwerftv1.UpgradeComponent]*kwerftv1.Upgrade{}
	for i := range items {
		u := &items[i]
		switch u.Status.Phase {
		case kwerftv1.UpgradeSucceeded, kwerftv1.UpgradeFailed, kwerftv1.UpgradeRolledBack:
		default:
			continue
		}
		if u.Status.FinishedAt == nil {
			continue
		}
		cur := latest[u.Spec.Component]
		if cur == nil || cur.Status.FinishedAt.Before(u.Status.FinishedAt) ||
			(cur.Status.FinishedAt.Equal(u.Status.FinishedAt) && cur.Name < u.Name) {
			latest[u.Spec.Component] = u
		}
	}
	var out []kwerftv1.Upgrade
	for _, c := range []kwerftv1.UpgradeComponent{kwerftv1.UpgradeKwerft, kwerftv1.UpgradeKubernetes} {
		u := latest[c]
		if u == nil || u.Status.Phase == kwerftv1.UpgradeSucceeded {
			continue
		}
		if cur, ok := running[c]; ok {
			if want, err := upgrades.ParseVersion(u.Spec.Version); err == nil && !cur.Less(want) {
				continue
			}
		}
		out = append(out, *u)
	}
	return out
}

// runningVersions are what runs now: this Kwerft, and the oldest kubelet,
// the version every node has reached. A version that does not parse is
// left out.
func runningVersions(kwerft string, nodes []corev1.Node) map[kwerftv1.UpgradeComponent]upgrades.Version {
	out := map[kwerftv1.UpgradeComponent]upgrades.Version{}
	if v, err := upgrades.ParseVersion(kwerft); err == nil {
		out[kwerftv1.UpgradeKwerft] = v
	}
	for i := range nodes {
		v, err := upgrades.ParseVersion(nodes[i].Status.NodeInfo.KubeletVersion)
		if err != nil {
			continue
		}
		if cur, ok := out[kwerftv1.UpgradeKubernetes]; !ok || v.Less(cur) {
			out[kwerftv1.UpgradeKubernetes] = v
		}
	}
	return out
}

// latestBuildResults says, per App, whether its latest finished build
// failed. Pull-request builds only report a check and are left out, as are
// cancelled ones; the order is the App's build number.
func latestBuildResults(items []kwerftv1.Build) map[client.ObjectKey]bool {
	latest := map[client.ObjectKey]*kwerftv1.Build{}
	for i := range items {
		b := &items[i]
		if b.Spec.Trigger == "pull-request" {
			continue
		}
		if b.Status.Phase != kwerftv1.BuildSucceeded && b.Status.Phase != kwerftv1.BuildFailed {
			continue
		}
		key := client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.App}
		if cur := latest[key]; cur == nil || b.Status.Number > cur.Status.Number {
			latest[key] = b
		}
	}
	out := make(map[client.ObjectKey]bool, len(latest))
	for k, b := range latest {
		out[k] = b.Status.Phase == kwerftv1.BuildFailed
	}
	return out
}

// watchBuildDurations observes kwerft_build_duration_seconds when a Build
// turns Succeeded or Failed. Builds that finished before this process
// started are not observed again (a histogram counts events, and those were
// counted by the previous process or are lost with it).
func watchBuildDurations(ctx context.Context, c interface {
	GetInformer(context.Context, client.Object, ...cache.InformerGetOption) (cache.Informer, error)
}) error {
	inf, err := c.GetInformer(ctx, &kwerftv1.Build{})
	if err != nil {
		return err
	}
	_, err = inf.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			old, ok1 := oldObj.(*kwerftv1.Build)
			b, ok2 := newObj.(*kwerftv1.Build)
			if ok1 && ok2 {
				observeBuild(old, b)
			}
		},
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

// observeBuild records b's duration if this update finished it.
func observeBuild(old, b *kwerftv1.Build) bool {
	finished := func(p kwerftv1.BuildPhase) bool { return p == kwerftv1.BuildSucceeded || p == kwerftv1.BuildFailed }
	if finished(old.Status.Phase) || !finished(b.Status.Phase) {
		return false
	}
	if b.Status.StartTime == nil || b.Status.CompletionTime == nil {
		return false
	}
	d := b.Status.CompletionTime.Sub(b.Status.StartTime.Time)
	if d < 0 {
		return false
	}
	result := "succeeded"
	if b.Status.Phase == kwerftv1.BuildFailed {
		result = "failed"
	}
	buildDuration.WithLabelValues(b.Namespace, b.Spec.App, result).Observe(d.Seconds())
	return true
}
