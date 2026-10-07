// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ehilzinger/kwerft/internal/metrics"
)

// diskVM answers the disk health queries with one worn-out, failing NVMe
// drive and a whole RAID1 on node disk-dedi-1.
func diskVM(t *testing.T) *metrics.Client {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		q := r.Form.Get("query")
		vector := func(v float64, labels ...string) {
			var l []string
			for i := 0; i+1 < len(labels); i += 2 {
				l = append(l, fmt.Sprintf("%q:%q", labels[i], labels[i+1]))
			}
			fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{%s},"value":[1700000000,"%g"]}]}}`, strings.Join(l, ","), v)
		}
		disk := []string{"node", "disk-dedi-1", "device", "nvme0"}
		md := []string{"nodename", "disk-dedi-1", "device", "md1"}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "node_md_disks_required"):
			vector(2, md...)
		case strings.Contains(q, "node_md_disks"):
			vector(2, append(md, "state", "active")...)
		case strings.Contains(q, "node_md_state"):
			vector(1, append(md, "state", "active")...)
		case strings.Contains(q, "node_md_blocks"):
			vector(100, md...)
		case strings.Contains(q, "smartctl_device_smart_status"):
			vector(0, disk...)
		case strings.Contains(q, "smartctl_device_critical_warning"):
			vector(4, disk...)
		case strings.Contains(q, "smartctl_device_percentage_used"):
			vector(115, disk...)
		case strings.Contains(q, "smartctl_devices{"):
			vector(1, "node", "disk-dedi-1")
		case strings.Contains(q, "smartctl_device{"):
			vector(1, append(disk, "model_name", "SAMSUNG MZVL2512HCJQ-00B00", "serial_number", "S64", "interface", "nvme")...)
		default:
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		}
	}))
	t.Cleanup(srv.Close)
	return metrics.New(srv.URL)
}

// The node list carries each node's disk health from the cluster's
// VictoriaMetrics: dedicated servers always (unknown without readings),
// Cloud servers only when they have a RAID or SMART readings.
func TestNodesDiskHealth(t *testing.T) {
	vm := diskVM(t)
	e := newNodesConsole(t, func(cfg *Config) { cfg.Metrics = vm })
	testNode(t, "disk-dedi-1", map[string]string{"kwerft.dev/platform": "dedicated"})
	testNode(t, "disk-dedi-2", map[string]string{"kwerft.dev/platform": "dedicated"})
	testNode(t, "disk-cloud-1", map[string]string{"kwerft.dev/platform": "cloud"})

	var list nodesJSON
	if code := e.c.owner.do(t, "GET", "/api/v1/clusters/local/nodes", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	if !list.DiskReadings {
		t.Fatalf("no disk readings: %+v", list)
	}
	byName := map[string]nodeJSON{}
	for _, n := range list.Nodes {
		byName[n.Name] = n
	}
	d1 := byName["disk-dedi-1"].DiskHealth
	if d1 == nil || d1.Health != healthBad || !d1.SMART || len(d1.Disks) != 1 || len(d1.Arrays) != 1 || d1.Disks[0].Serial != "S64" ||
		d1.Arrays[0].Health != healthOK || d1.Summary != "1 disk failing" {
		t.Fatalf("disk-dedi-1: %+v", d1)
	}
	if d2 := byName["disk-dedi-2"].DiskHealth; d2 == nil || d2.Health != healthUnknown {
		t.Errorf("disk-dedi-2: %+v", d2)
	}
	if d := byName["disk-cloud-1"].DiskHealth; d != nil {
		t.Errorf("disk-cloud-1: %+v", d)
	}
}
