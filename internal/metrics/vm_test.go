package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/yaml"
)

// TestAgainstVictoriaMetrics checks the recording rules, the catalog and the
// confinement against a real VictoriaMetrics, which it fills with what
// kube-state-metrics, the kubelet, node-exporter, Traefik and Kwerft
// export. It writes the recording rules' results back like vmalert would.
// Opt-in, since CI has no VictoriaMetrics:
//
//	victoria-metrics -storageDataPath=/tmp/vm -httpListenAddr=127.0.0.1:18428 -search.latencyOffset=1s
//	KWERFT_TEST_VICTORIAMETRICS=http://127.0.0.1:18428 go test ./internal/metrics -run VictoriaMetrics -v
//
// Use an empty storage directory: the test checks exact results.
func TestAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("KWERFT_TEST_VICTORIAMETRICS")
	if base == "" {
		t.Skip("KWERFT_TEST_VICTORIAMETRICS not set")
	}
	ctx := context.Background()
	c := New(base)
	now := time.Now().Truncate(time.Minute).Add(-2 * time.Minute)

	// Ten minutes of samples, every 30s, ending at now.
	var buf bytes.Buffer
	sample := func(series string, v func(i int) float64) {
		for i := 0; i <= 20; i++ {
			ts := now.Add(time.Duration(i-20) * 30 * time.Second)
			fmt.Fprintf(&buf, "%s %g %d\n", series, v(i), ts.UnixMilli())
		}
	}
	constant := func(x float64) func(int) float64 { return func(int) float64 { return x } }
	perSecond := func(x float64) func(int) float64 { return func(i int) float64 { return x * 30 * float64(i) } }
	pod := func(ns, pod string) string { return fmt.Sprintf(`namespace="%s",pod="%s"`, ns, pod) }

	// kube-state-metrics (pod labels through metricLabelsAllowlist).
	sample(`kube_pod_labels{`+pod("shop", "api-1")+`,label_kwerft_dev_app="api",label_kwerft_dev_project="shop"}`, constant(1))
	sample(`kube_pod_labels{`+pod("shop", "api-2")+`,label_kwerft_dev_app="api",label_kwerft_dev_project="shop"}`, constant(1))
	sample(`kube_pod_labels{`+pod("shop", "nightly-x")+`,label_kwerft_dev_project="shop"}`, constant(1))
	sample(`kube_pod_labels{`+pod("kube-system", "coredns-1")+`}`, constant(1))
	sample(`kube_pod_container_resource_limits{`+pod("shop", "api-1")+`,container="app",resource="memory",unit="byte"}`, constant(256e6))
	sample(`kube_pod_container_resource_limits{`+pod("shop", "api-2")+`,container="app",resource="memory",unit="byte"}`, constant(256e6))
	sample(`kube_pod_container_status_restarts_total{`+pod("shop", "api-1")+`,container="app"}`, func(i int) float64 { return float64(i / 10) })
	sample(`kube_pod_container_status_restarts_total{`+pod("shop", "api-2")+`,container="app"}`, constant(0))
	// kubelet (cAdvisor): a pod-level series without container is left out.
	sample(`container_cpu_usage_seconds_total{`+pod("shop", "api-1")+`,container="app",job="kubelet"}`, perSecond(0.5))
	sample(`container_cpu_usage_seconds_total{`+pod("shop", "api-1")+`,container="",job="kubelet"}`, perSecond(9))
	sample(`container_cpu_usage_seconds_total{`+pod("shop", "api-2")+`,container="app",job="kubelet"}`, perSecond(0.25))
	sample(`container_cpu_usage_seconds_total{`+pod("shop", "nightly-x")+`,container="task",job="kubelet"}`, perSecond(1))
	sample(`container_cpu_usage_seconds_total{`+pod("kube-system", "coredns-1")+`,container="coredns",job="kubelet"}`, perSecond(0.1))
	sample(`container_memory_working_set_bytes{`+pod("shop", "api-1")+`,container="app"}`, constant(100e6))
	sample(`container_memory_working_set_bytes{`+pod("shop", "api-2")+`,container="app"}`, constant(50e6))
	sample(`container_memory_working_set_bytes{`+pod("kube-system", "coredns-1")+`,container="coredns"}`, constant(20e6))
	// node-exporter, with the scrape's own namespace label.
	node := `instance="10.0.0.2:9100",job="node-exporter",namespace="kwerft-observability"`
	sample(`node_uname_info{`+node+`,nodename="node-1"}`, constant(1))
	for cpu := range 4 {
		sample(fmt.Sprintf(`node_cpu_seconds_total{%s,cpu="%d",mode="idle"}`, node, cpu), perSecond(0.75))
		sample(fmt.Sprintf(`node_cpu_seconds_total{%s,cpu="%d",mode="user"}`, node, cpu), perSecond(0.25))
	}
	sample(`node_memory_MemTotal_bytes{`+node+`}`, constant(8e9))
	sample(`node_memory_MemAvailable_bytes{`+node+`}`, constant(6e9))
	sample(`node_filesystem_size_bytes{`+node+`,mountpoint="/",fstype="ext4",device="/dev/sda1"}`, constant(100e9))
	sample(`node_filesystem_avail_bytes{`+node+`,mountpoint="/",fstype="ext4",device="/dev/sda1"}`, constant(60e9))
	// Traefik routers (namespace is the Traefik pod's, from the scrape).
	router := func(route, ep string) string {
		return fmt.Sprintf(`router="httproute-%s-gw-kwerft-system-kwerft-ep-%s-0-0123456789@kubernetesgateway"`, route, ep)
	}
	tr := `namespace="traefik",method="GET",protocol="http",service="x"`
	sample(`traefik_router_requests_total{`+tr+`,code="200",`+router("shop-api-8080", "websecure")+`}`, perSecond(10))
	sample(`traefik_router_requests_total{`+tr+`,code="503",`+router("shop-api-8080", "websecure")+`}`, perSecond(1))
	sample(`traefik_router_requests_total{`+tr+`,code="301",`+router("shop-api-8080-redirect", "web")+`}`, perSecond(5))
	sample(`traefik_router_requests_total{`+tr+`,code="200",`+router("kwerft-system-kwerft", "websecure")+`}`, perSecond(3))
	for le, n := range map[string]float64{"0.05": 6, "0.1": 9, "0.25": 10, "+Inf": 10} {
		sample(`traefik_router_request_duration_seconds_bucket{`+tr+`,code="200",le="`+le+`",`+router("shop-api-8080", "websecure")+`}`, perSecond(n))
	}
	// Kwerft's own (internal/controllers/metrics.go), scraped with honorLabels.
	sample(`kwerft_http_route_info{namespace="shop",app="api",route="httproute-shop-api-8080",job="kwerft",pod="kwerft-0"}`, constant(1))
	sample(`up{namespace="kube-system",job="apiserver"}`, constant(1))
	sample(`up{namespace="shop",job="x"}`, constant(1))

	post(t, base+"/api/v1/import/prometheus", "text/plain", buf.Bytes())
	flush(t, base)

	// Evaluate the chart's recording rules over the window and write the
	// results back, as vmalert does.
	rules := chartRules(t)
	results := map[string][]Series{}
	window := Range{Start: now.Add(-4 * time.Minute), End: now, Step: 30 * time.Second}
	var lines bytes.Buffer
	for _, r := range rules {
		s, err := c.QueryRange(ctx, r.Expr, window, Unconfined())
		if err != nil {
			t.Fatalf("rule %s: %v", r.Record, err)
		}
		results[r.Record] = s
		for _, ser := range s {
			labels := map[string]string{"__name__": r.Record}
			for k, v := range ser.Labels {
				labels[k] = v
			}
			var vals []float64
			var tss []int64
			for _, p := range ser.Points {
				vals = append(vals, p.V)
				tss = append(tss, p.T*1000)
			}
			line, _ := json.Marshal(map[string]any{"metric": labels, "values": vals, "timestamps": tss})
			lines.Write(append(line, '\n'))
		}
	}
	post(t, base+"/api/v1/import", "application/json", lines.Bytes())
	flush(t, base)

	at := func(s []Series, labels map[string]string) float64 {
		t.Helper()
		for _, ser := range s {
			match := len(ser.Labels) == len(labels)
			for k, v := range labels {
				match = match && ser.Labels[k] == v
			}
			if match && len(ser.Points) > 0 {
				return ser.Points[len(ser.Points)-1].V
			}
		}
		t.Fatalf("no series %v in %+v", labels, s)
		return 0
	}
	near := func(what string, got, want float64) {
		t.Helper()
		if got < want*0.98 || got > want*1.02 {
			t.Errorf("%s = %g, want %g", what, got, want)
		}
	}

	cpu := results[RecordCPU]
	if len(cpu) != 2 {
		t.Errorf("%s: %+v (want the two api pods only)", RecordCPU, cpu)
	}
	near("cpu api-1", at(cpu, map[string]string{"namespace": "shop", "app": "api", "pod": "api-1"}), 0.5)
	near("memory api-2", at(results[RecordMemory], map[string]string{"namespace": "shop", "app": "api", "pod": "api-2"}), 50e6)
	req := results[RecordHTTPRequests]
	if len(req) != 2 {
		t.Errorf("%s: %+v (redirect and console routes must drop out)", RecordHTTPRequests, req)
	}
	near("2xx", at(req, map[string]string{"namespace": "shop", "app": "api", "code_class": "2xx"}), 10)
	near("5xx", at(req, map[string]string{"namespace": "shop", "app": "api", "code_class": "5xx"}), 1)
	p95 := at(results[RecordHTTPLatencyP95], map[string]string{"namespace": "shop", "app": "api"})
	if p95 <= 0.1 || p95 > 0.25 {
		t.Errorf("p95 = %g, want within the (0.1, 0.25] bucket", p95)
	}

	// The per-app catalog, confined to the project.
	exprs, err := AppQueries("shop", "api", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	app := map[string]float64{}
	for _, k := range AppSeries {
		s, err := c.QueryRange(ctx, exprs[k], window, Namespaces("shop"))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if len(s) != 1 || len(s[0].Points) == 0 {
			t.Fatalf("%s: %+v", k, s)
		}
		app[k] = s[0].Points[len(s[0].Points)-1].V
	}
	near("cpu", app["cpu"], 0.75)
	near("memory", app["memory"], 150e6)
	near("memoryLimit", app["memoryLimit"], 512e6)
	near("requests", app["requests"], 11)
	near("errors", app["errors"], 1)
	if app["latencyP95"] <= 0.1 || app["latencyP95"] > 0.25 {
		t.Errorf("latencyP95 = %g", app["latencyP95"])
	}

	// Overview.
	for q, want := range map[string]float64{NodeCPU: 1, NodeCPUCapacity: 4, NodeMemory: 2e9, NodeMemoryCapacity: 8e9, NodeDisk: 40e9, NodeDiskCapacity: 100e9} {
		s, err := c.Query(ctx, q, now, Unconfined())
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		near(q, at(s, map[string]string{"nodename": "node-1"}), want)
	}
	s, err := c.Query(ctx, PlatformMemory, now, Unconfined())
	if err != nil {
		t.Fatal(err)
	}
	near("platform memory", at(s, map[string]string{"namespace": "kube-system"}), 20e6)
	if s, err = c.Query(ctx, AppsMemory, now, Namespaces("shop")); err != nil {
		t.Fatal(err)
	}
	near("apps memory", at(s, map[string]string{"namespace": "shop", "app": "api"}), 150e6)

	// Confinement: a selector naming another namespace, or none, still only
	// sees the allowed namespaces; node metrics carry the exporter's
	// namespace and stay hidden.
	for _, q := range []string{`up{namespace="kube-system"}`, `count(node_uname_info)`, NodeMemory, PlatformMemory,
		`kube_pod_labels{namespace=~"kube-.*"}`, `sum(rate(container_cpu_usage_seconds_total{namespace="kube-system"}[5m]))`} {
		s, err := c.Query(ctx, q, now, Namespaces("shop"))
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		if len(s) != 0 {
			t.Errorf("confined %s returned %+v", q, s)
		}
	}
	all, err := c.Query(ctx, `{__name__=~".+"}`, now, Namespaces("shop"))
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal(`{__name__=~".+"} returned nothing`)
	}
	for _, ser := range all {
		if ser.Labels["namespace"] != "shop" {
			t.Errorf("confined {__name__=~\".+\"} returned %v", ser.Labels)
		}
	}
	// Two allowed namespaces.
	if s, err = c.Query(ctx, `up`, now, Namespaces("shop", "blog")); err != nil || len(s) != 1 {
		t.Errorf("up in shop|blog: %+v %v", s, err)
	}
}

type rule struct {
	Record string `json:"record"`
	Expr   string `json:"expr"`
}

// chartRules reads the recording rules from the chart template (template
// lines dropped, the Gateway namespace is literal).
func chartRules(t *testing.T) []rule {
	raw, err := os.ReadFile(filepath.Join("..", "..", "charts", "kwerft", "templates", "metrics-rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.Contains(l, "{{") {
			kept = append(kept, l)
		}
	}
	var doc struct {
		Spec struct {
			Groups []struct {
				Rules []rule `json:"rules"`
			} `json:"groups"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(kept, "\n")), &doc); err != nil {
		t.Fatal(err)
	}
	var out []rule
	for _, g := range doc.Spec.Groups {
		out = append(out, g.Rules...)
	}
	var names []string
	for _, r := range out {
		names = append(names, r.Record)
	}
	for _, r := range Records {
		if !slices.Contains(names, r) {
			t.Fatalf("rule %s missing", r)
		}
	}
	return out
}

func post(t *testing.T, url, contentType string, body []byte) {
	t.Helper()
	res, err := http.Post(url, contentType, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		t.Fatalf("POST %s: %s", url, res.Status)
	}
}

func flush(t *testing.T, base string) {
	t.Helper()
	res, err := http.Get(base + "/internal/force_flush")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	time.Sleep(2 * time.Second) // new series become searchable shortly after
}
