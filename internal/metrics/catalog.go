package metrics

import (
	"fmt"
	"strings"
	"time"
)

// Recording rules from the chart (charts/kwerft/templates/metrics-rules.yaml),
// so charts and alerts use Kwerft's labels — namespace, app, pod — instead of
// Traefik's router names or the kubelet's.
const (
	// RecordHTTPRequests: requests per second through Traefik, by namespace,
	// app and code_class (1xx..5xx).
	RecordHTTPRequests = "kwerft:http_requests:rate5m"
	// RecordHTTPLatencyP95: 95th percentile response time, by namespace and app.
	RecordHTTPLatencyP95 = "kwerft:http_latency_p95_seconds:5m"
	// RecordMemory: working set bytes, by namespace, app and pod.
	RecordMemory = "kwerft:container_memory_working_set_bytes"
	// RecordCPU: CPU cores in use, by namespace, app and pod.
	RecordCPU = "kwerft:container_cpu_usage_cores:rate5m"
)

// Records lists every recording rule the catalog relies on.
var Records = []string{RecordHTTPRequests, RecordHTTPLatencyP95, RecordMemory, RecordCPU}

// AppSeries are the per-app charts, in display order; the keys of the app
// metrics response.
var AppSeries = []string{"cpu", "memory", "memoryLimit", "restarts", "requests", "errors", "latencyP95"}

// appPods selects one App's pods by the label kube-state-metrics exports
// from kwerft.dev/app (metricLabelsAllowlist in install.sh).
const appPods = `max by (namespace, pod) (kube_pod_labels{namespace="%[1]s", label_kwerft_dev_app="%[2]s"})`

// AppQueries are the per-app charts for App app in namespace (a project).
// window is the chart's step: restarts count what happened within it.
func AppQueries(namespace, app string, window time.Duration) (map[string]string, error) {
	if !dnsLabel.MatchString(namespace) || !dnsLabel.MatchString(app) {
		return nil, fmt.Errorf("invalid project or app name")
	}
	sel := fmt.Sprintf(`{namespace="%s", app="%s"}`, namespace, app)
	pods := fmt.Sprintf(appPods, namespace, app)
	w := promDuration(max(window, time.Minute))
	return map[string]string{
		"cpu":    "sum(" + RecordCPU + sel + ")",
		"memory": "sum(" + RecordMemory + sel + ")",
		"memoryLimit": fmt.Sprintf(`sum(kube_pod_container_resource_limits{namespace="%s", resource="memory"} * on (namespace, pod) group_left () %s)`,
			namespace, pods),
		"restarts": fmt.Sprintf(`sum(increase(kube_pod_container_status_restarts_total{namespace="%s"}[%s]) * on (namespace, pod) group_left () %s)`,
			namespace, w, pods),
		"requests": "sum(" + RecordHTTPRequests + sel + ")",
		// No 5xx at all is 0, not "no data", as long as there is traffic.
		"errors": fmt.Sprintf(`sum(%s{namespace="%s", app="%s", code_class="5xx"}) or (0 * sum(%s%s))`,
			RecordHTTPRequests, namespace, app, RecordHTTPRequests, sel),
		"latencyP95": "max(" + RecordHTTPLatencyP95 + sel + ")",
	}, nil
}

// promDuration writes d the way PromQL reads it.
func promDuration(d time.Duration) string {
	return fmt.Sprintf("%ds", int64(d/time.Second))
}

// platformNamespaces matches PlatformNamespaces and kube-*, kwerft-*.
var platformRE = func() string {
	var parts []string
	for _, n := range PlatformNamespaces {
		if !strings.HasPrefix(n, "kube-") && !strings.HasPrefix(n, "kwerft-") {
			parts = append(parts, n)
		}
	}
	return strings.Join(append(parts, "kube-.*", "kwerft-.*"), "|")
}()

// byNode joins node-exporter series to the node's name (node_uname_info's
// nodename, the hostname k3s uses as node name).
func byNode(expr string) string {
	return fmt.Sprintf(`sum by (nodename) ((%s) * on (instance) group_left (nodename) node_uname_info)`, expr)
}

// Overview queries. Node and platform queries are for owners and admins
// (an unconfined scope); the app queries work in any scope, the confinement
// keeps them to the user's projects.
var (
	NodeCPU            = byNode(`sum by (instance) (rate(node_cpu_seconds_total{mode!~"idle|iowait|steal"}[5m]))`)
	NodeCPUCapacity    = byNode(`count by (instance) (node_cpu_seconds_total{mode="idle"})`)
	NodeMemory         = byNode(`node_memory_MemTotal_bytes - node_memory_MemAvailable_bytes`)
	NodeMemoryCapacity = byNode(`node_memory_MemTotal_bytes`)
	NodeDisk           = byNode(`node_filesystem_size_bytes{mountpoint="/"} - node_filesystem_avail_bytes{mountpoint="/"}`)
	NodeDiskCapacity   = byNode(`node_filesystem_size_bytes{mountpoint="/"}`)

	PlatformCPU    = `sum by (namespace) (rate(container_cpu_usage_seconds_total{container!="", container!="POD", namespace=~"` + platformRE + `"}[5m]))`
	PlatformMemory = `sum by (namespace) (container_memory_working_set_bytes{container!="", container!="POD", namespace=~"` + platformRE + `"})`

	AppsCPU    = `sum by (namespace, app) (` + RecordCPU + `)`
	AppsMemory = `sum by (namespace, app) (` + RecordMemory + `)`

	// Volume fill per claim (the Overview map); any scope.
	VolumeUsed     = `sum by (namespace, persistentvolumeclaim) (kubelet_volume_stats_used_bytes)`
	VolumeCapacity = `sum by (namespace, persistentvolumeclaim) (kubelet_volume_stats_capacity_bytes)`
)
