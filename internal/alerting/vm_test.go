// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package alerting

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/metrics"
)

// TestDiskAlertsAgainstVictoriaMetrics evaluates the disk alerts and the
// console's disk health queries against a real VictoriaMetrics filled with
// what node-exporter, kube-state-metrics and the chart's smartctl_exporter
// (after its scrape's relabeling) export. Opt-in, since CI has no
// VictoriaMetrics:
//
//	victoria-metrics -storageDataPath=/tmp/vm -httpListenAddr=127.0.0.1:18428 -search.latencyOffset=1s
//	KWERFT_TEST_VICTORIAMETRICS=http://127.0.0.1:18428 go test ./internal/alerting -run VictoriaMetrics -v
//
// Use an empty storage directory: the test checks exact results.
func TestDiskAlertsAgainstVictoriaMetrics(t *testing.T) {
	base := os.Getenv("KWERFT_TEST_VICTORIAMETRICS")
	if base == "" {
		t.Skip("KWERFT_TEST_VICTORIAMETRICS not set")
	}
	ctx := context.Background()
	c := metrics.New(base)
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

	// Nodes: three dedicated servers and a Cloud server (kube-state-metrics,
	// with the node label install.sh allows).
	for node, platform := range map[string]string{"dedi-a": "dedicated", "dedi-b": "dedicated", "dedi-c": "dedicated", "cloud-1": "cloud"} {
		sample(fmt.Sprintf(`kube_node_labels{namespace="kwerft-observability",pod="ksm-1",node=%q,label_kwerft_dev_platform=%q}`, node, platform), constant(1))
	}
	// node-exporter on dedi-a, dedi-b and cloud-1 (not on dedi-c), joined to
	// their nodes through kube_pod_info.
	exporter := func(node, ip string) string {
		pod := "ne-" + node
		sample(fmt.Sprintf(`kube_pod_info{namespace="kwerft-observability",pod=%q,node=%q}`, pod, node), constant(1))
		target := fmt.Sprintf(`namespace="kwerft-observability",pod=%q,instance="%s:9100",job="node-exporter"`, pod, ip)
		sample(`up{`+target+`}`, constant(1))
		sample(`node_uname_info{`+target+`,nodename="`+node+`"}`, constant(1))
		return target
	}
	md := func(target, dev string, active, failed, required float64, state string) {
		for s, v := range map[string]float64{"active": active, "failed": failed, "spare": 0} {
			sample(fmt.Sprintf(`node_md_disks{%s,device=%q,state=%q}`, target, dev, s), constant(v))
		}
		sample(fmt.Sprintf(`node_md_disks_required{%s,device=%q}`, target, dev), constant(required))
		for _, s := range []string{"active", "inactive", "recovering", "resync", "check"} {
			v := 0.0
			if s == state {
				v = 1
			}
			sample(fmt.Sprintf(`node_md_state{%s,device=%q,state=%q}`, target, dev, s), constant(v))
		}
		sample(fmt.Sprintf(`node_md_blocks{%s,device=%q}`, target, dev), constant(1000))
		synced := 1000.0
		if state == "recovering" {
			synced = 434
		}
		sample(fmt.Sprintf(`node_md_blocks_synced{%s,device=%q}`, target, dev), constant(synced))
	}
	a := exporter("dedi-a", "10.0.0.1")
	md(a, "md0", 2, 0, 2, "active")
	md(a, "md1", 2, 0, 2, "active")
	b := exporter("dedi-b", "10.0.0.2")
	md(b, "md0", 1, 0, 2, "recovering")
	exporter("cloud-1", "10.0.0.3")

	// SMART readings as vmagent stores them: job and node from the scrape.
	smart := func(node, metric, dev string, v func(int) float64) {
		sample(fmt.Sprintf(`%s{job="kwerft-disk-health",namespace="kwerft-system",pod="dh-%s",node=%q,device=%q}`, metric, node, node, dev), v)
	}
	disk := func(node, dev string, status, warning, used float64, mediaErrors func(int) float64, exit float64) {
		smart(node, "smartctl_device_smart_status", dev, constant(status))
		smart(node, "smartctl_device_critical_warning", dev, constant(warning))
		smart(node, "smartctl_device_percentage_used", dev, constant(used))
		smart(node, "smartctl_device_available_spare", dev, constant(100))
		smart(node, "smartctl_device_available_spare_threshold", dev, constant(10))
		smart(node, "smartctl_device_media_errors", dev, mediaErrors)
		smart(node, "smartctl_device_smartctl_exit_status", dev, constant(exit))
	}
	// dedi-a: nvme0 as on kwerft-dedi-1 (2026-10-06): worn out (status
	// FAILED from critical warning 0x04 alone, spare blocks at 100 %), which
	// is DiskWearing, not DiskFailing; nvme1 healthy but with new media
	// errors; nvme2 worn and below its spare threshold (warning 0x05); only
	// nvme0 has its identity series.
	disk("dedi-a", "nvme0", 0, 4, 115, constant(0), 8)
	disk("dedi-a", "nvme1", 1, 0, 20, func(i int) float64 { return float64(2 + i/10) }, 0)
	disk("dedi-a", "nvme2", 0, 5, 60, constant(0), 8)
	sample(`smartctl_device{job="kwerft-disk-health",node="dedi-a",device="nvme0",model_name="SAMSUNG MZVL2512HCJQ-00B00",serial_number="S64",interface="nvme"}`, constant(1))
	sample(`smartctl_devices{job="kwerft-disk-health",node="dedi-a"}`, constant(2))
	// dedi-b: an old media error count (no alert), a disk smartctl
	// cannot open, and a SATA disk with status FAILED (no critical warning
	// series).
	disk("dedi-b", "sda", 1, 0, 0, constant(7), 0)
	smart("dedi-b", "smartctl_device_smart_status", "sde", constant(0))
	smart("dedi-b", "smartctl_device_smartctl_exit_status", "sdb", constant(2))
	// sdc's exporter pod was replaced halfway: the new pod's series starts
	// at the old count, which is no new media error.
	disk("dedi-b", "sdc", 1, 0, 0, constant(3), 0)
	for i := 0; i < 10; i++ {
		ts := now.Add(time.Duration(i-20) * 30 * time.Second)
		fmt.Fprintf(&buf, `smartctl_device_media_errors{job="kwerft-disk-health",namespace="kwerft-system",pod="dh-old",node="dedi-b",device="sdc"} 3 %d`+"\n", ts.UnixMilli())
	}
	sample(`smartctl_devices{job="kwerft-disk-health",node="dedi-b"}`, constant(2))

	res, err := http.Post(base+"/api/v1/import/prometheus", "text/plain", &buf)
	if err != nil || res.StatusCode/100 != 2 {
		t.Fatalf("import: %v %v", res, err)
	}
	res.Body.Close()
	if res, err := http.Get(base + "/internal/force_flush"); err == nil {
		res.Body.Close()
	}
	time.Sleep(2 * time.Second)

	firing := func(cond kwerftv1.AlertCondition, keys ...string) []string {
		t.Helper()
		expr := Expr(&kwerftv1.AlertRuleSpec{Condition: cond})
		got, err := c.Query(ctx, expr, now, metrics.Unconfined())
		if err != nil {
			t.Fatalf("%s: %v\n%s", cond, err, expr)
		}
		var out []string
		for _, s := range got {
			var parts []string
			for _, k := range keys {
				parts = append(parts, s.Labels[k])
			}
			out = append(out, strings.Join(parts, "/"))
		}
		slices.Sort(out)
		return out
	}
	if got := firing(kwerftv1.AlertRAIDDegraded, "node", "device"); !slices.Equal(got, []string{"dedi-b/md0"}) {
		t.Errorf("RAIDDegraded: %v", got)
	}
	if got := firing(kwerftv1.AlertDiskFailing, "node", "device", "serial_number"); !slices.Equal(got, []string{"dedi-a/nvme1/", "dedi-a/nvme2/", "dedi-b/sde/"}) {
		t.Errorf("DiskFailing: %v", got)
	}
	wear, err := c.Query(ctx, Expr(&kwerftv1.AlertRuleSpec{Condition: kwerftv1.AlertDiskWearing}), now, metrics.Unconfined())
	if err != nil || len(wear) != 1 || wear[0].Labels["device"] != "nvme0" || wear[0].Labels["serial_number"] != "S64" || wear[0].Points[0].V != 115 {
		t.Errorf("DiskWearing: %+v %v", wear, err)
	}
	if got := firing(kwerftv1.AlertDiskReadingsMissing, "node", "device"); !slices.Equal(got, []string{"dedi-b/sdb", "dedi-c/"}) {
		t.Errorf("DiskReadingsMissing: %v", got)
	}

	// The console's queries (Clusters › Nodes) return what it assembles.
	for _, q := range []struct {
		expr string
		want int
	}{
		{metrics.RAIDDisks, 9}, {metrics.RAIDRequired, 3}, {metrics.RAIDState, 3}, {metrics.RAIDSynced, 3},
		{metrics.SMARTStatus, 6}, {metrics.SMARTPercentageUsed, 5}, {metrics.SMARTExitStatus, 6}, {metrics.SMARTDevice, 1}, {metrics.SMARTDevices, 2},
	} {
		got, err := c.Query(ctx, q.expr, now, metrics.Unconfined())
		if err != nil || len(got) != q.want {
			t.Errorf("%s: %d series, want %d (%v)", q.expr, len(got), q.want, err)
		}
		for _, s := range got {
			if s.Labels["nodename"] == "" && s.Labels["node"] == "" {
				t.Errorf("%s: a series without its node: %v", q.expr, s.Labels)
			}
		}
	}
	synced, _ := c.Query(ctx, metrics.RAIDSynced+` < 100`, now, metrics.Unconfined())
	if len(synced) != 1 || synced[0].Labels["nodename"] != "dedi-b" || int(synced[0].Points[0].V) != 43 {
		t.Errorf("synced: %+v", synced)
	}
}
