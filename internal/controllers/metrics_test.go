package controllers

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func ts(sec int64) *metav1.Time { t := metav1.NewTime(time.Unix(sec, 0)); return &t }

func build(ns, name, app, trigger string, number int64, phase kwerftv1.BuildPhase) *kwerftv1.Build {
	return &kwerftv1.Build{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       kwerftv1.BuildSpec{App: app, Trigger: trigger},
		Status:     kwerftv1.BuildStatus{Number: number, Phase: phase},
	}
}

func route(ns, name, app string, backend bool) *gwv1.HTTPRoute {
	r := &gwv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
		Labels: map[string]string{LabelApp: app, LabelManagedBy: ManagedByKwerft}}}
	rule := gwv1.HTTPRouteRule{}
	if backend {
		rule.BackendRefs = []gwv1.HTTPBackendRef{{BackendRef: gwv1.BackendRef{BackendObjectReference: gwv1.BackendObjectReference{Name: gwv1.ObjectName(app)}}}}
	}
	r.Spec.Rules = []gwv1.HTTPRouteRule{rule}
	return r
}

func TestMetricsCollector(t *testing.T) {
	objs := []client.Object{
		&kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "nightly"},
			Status: kwerftv1.ScheduleStatus{LastSuccessTime: ts(1700000000), LastFailureTime: ts(1690000000)}},
		&kwerftv1.Schedule{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "never-ran"}},
		// api: #3 failed is the latest finished; #4 still runs, #5 is a PR check.
		build("shop", "api-1", "api", "push", 1, kwerftv1.BuildSucceeded),
		build("shop", "api-3", "api", "push", 3, kwerftv1.BuildFailed),
		build("shop", "api-4", "api", "manual", 4, kwerftv1.BuildRunning),
		build("shop", "api-5", "api", "pull-request", 5, kwerftv1.BuildSucceeded),
		// web: fixed by the next build; a cancelled one does not count.
		build("shop", "web-1", "web", "push", 1, kwerftv1.BuildFailed),
		build("shop", "web-2", "web", "push", 2, kwerftv1.BuildSucceeded),
		build("shop", "web-3", "web", "push", 3, kwerftv1.BuildCancelled),
		&kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "shop.example.com"},
			Spec: kwerftv1.DomainSpec{Hostname: "shop.example.com"}, Status: kwerftv1.DomainStatus{NotAfter: ts(1710000000)}},
		&kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "pending.example.com"},
			Spec: kwerftv1.DomainSpec{Hostname: "pending.example.com"}},
		&kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName},
			Status: kwerftv1.ConsoleSettingsStatus{Certificates: []kwerftv1.CertificateState{
				{Name: "console", Purpose: "console", Hostnames: []string{"ops.example.com"}, NotAfter: ts(1720000000)},
				{Name: "wildcard", Purpose: "apps-wildcard", Hostnames: []string{"*.apps.example.com"}, NotAfter: ts(1730000000)},
				{Name: "next", Purpose: "console-next", Hostnames: []string{"new.example.com"}},
			}}},
		route("shop", "api-8080", "api", true),
		route("shop", "api-8080-redirect", "api", false),
	}
	c := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(objs...).Build()
	want := `
# HELP kwerft_app_latest_build_failed 1 when the App's latest finished build (pull-request checks aside) failed, 0 when it succeeded.
# TYPE kwerft_app_latest_build_failed gauge
kwerft_app_latest_build_failed{app="api",namespace="shop"} 1
kwerft_app_latest_build_failed{app="web",namespace="shop"} 0
# HELP kwerft_console_certificate_expiry_timestamp_seconds When a certificate the console depends on expires, per hostname it covers.
# TYPE kwerft_console_certificate_expiry_timestamp_seconds gauge
kwerft_console_certificate_expiry_timestamp_seconds{hostname="*.apps.example.com",purpose="apps-wildcard"} 1.73e+09
kwerft_console_certificate_expiry_timestamp_seconds{hostname="ops.example.com",purpose="console"} 1.72e+09
# HELP kwerft_domain_certificate_expiry_timestamp_seconds When the certificate serving the Domain expires (status.notAfter).
# TYPE kwerft_domain_certificate_expiry_timestamp_seconds gauge
kwerft_domain_certificate_expiry_timestamp_seconds{domain="shop.example.com",hostname="shop.example.com",namespace="shop"} 1.71e+09
# HELP kwerft_http_route_info Maps Traefik's router names (route label: httproute-<namespace>-<HTTPRoute>) to the App an HTTPRoute serves; value 1.
# TYPE kwerft_http_route_info gauge
kwerft_http_route_info{app="api",namespace="shop",route="httproute-shop-api-8080"} 1
# HELP kwerft_schedule_last_failure_timestamp_seconds When a run of the Schedule last failed (status.lastFailureTime).
# TYPE kwerft_schedule_last_failure_timestamp_seconds gauge
kwerft_schedule_last_failure_timestamp_seconds{namespace="shop",schedule="nightly"} 1.69e+09
# HELP kwerft_schedule_last_success_timestamp_seconds When a run of the Schedule last succeeded (status.lastSuccessTime).
# TYPE kwerft_schedule_last_success_timestamp_seconds gauge
kwerft_schedule_last_success_timestamp_seconds{namespace="shop",schedule="nightly"} 1.7e+09
`
	if err := testutil.CollectAndCompare(&MetricsCollector{Reader: c}, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestObserveBuild(t *testing.T) {
	buildDuration.Reset()
	running := build("shop", "api-7", "api", "push", 7, kwerftv1.BuildRunning)
	done := running.DeepCopy()
	done.Status.Phase = kwerftv1.BuildFailed
	done.Status.StartTime, done.Status.CompletionTime = ts(1700000000), ts(1700000075)

	if !observeBuild(running, done) {
		t.Fatal("finishing build not observed")
	}
	if observeBuild(done, done) {
		t.Fatal("an update of a finished build observed again")
	}
	noTimes := done.DeepCopy()
	noTimes.Status.StartTime = nil
	if observeBuild(running, noTimes) {
		t.Fatal("observed without a start time")
	}
	want := `
# HELP kwerft_build_duration_seconds How long builds ran, from start to completion, by result (succeeded, failed).
# TYPE kwerft_build_duration_seconds histogram
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="15"} 0
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="30"} 0
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="60"} 0
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="90"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="120"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="180"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="300"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="600"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="900"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="1800"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="3600"} 1
kwerft_build_duration_seconds_bucket{app="api",namespace="shop",result="failed",le="+Inf"} 1
kwerft_build_duration_seconds_sum{app="api",namespace="shop",result="failed"} 75
kwerft_build_duration_seconds_count{app="api",namespace="shop",result="failed"} 1
`
	if err := testutil.CollectAndCompare(buildDuration, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}
