// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"slices"
	"testing"

	"github.com/ehilzinger/kwerft/internal/metrics"
)

func diskSeries(v float64, kv ...string) metrics.Series {
	l := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return metrics.Series{Labels: l, Points: []metrics.Point{{T: 1, V: v}}}
}

// The readings of kwerft-dedi-1 on 2026-10-06 (both NVMe drives worn out,
// SMART failed, critical warning 0x04, the RAID still whole), a node whose
// RAID lost a disk and is rebuilding, a dedicated server without readings
// and a Cloud server without any.
func TestDiskHealth(t *testing.T) {
	res := map[string][]metrics.Series{}
	add := func(key string, s ...metrics.Series) { res[key] = append(res[key], s...) }
	for _, md := range []string{"md0", "md1"} {
		add("raidDisks", diskSeries(2, "nodename", "dedi-1", "device", md, "state", "active"), diskSeries(0, "nodename", "dedi-1", "device", md, "state", "failed"),
			diskSeries(0, "nodename", "dedi-1", "device", md, "state", "spare"))
		add("raidRequired", diskSeries(2, "nodename", "dedi-1", "device", md))
		add("raidState", diskSeries(1, "nodename", "dedi-1", "device", md, "state", "active"))
		add("raidSynced", diskSeries(100, "nodename", "dedi-1", "device", md))
	}
	add("raidDisks", diskSeries(1, "nodename", "dedi-2", "device", "md2", "state", "active"))
	add("raidRequired", diskSeries(2, "nodename", "dedi-2", "device", "md2"))
	add("raidState", diskSeries(1, "nodename", "dedi-2", "device", "md2", "state", "recovering"))
	add("raidSynced", diskSeries(43.4, "nodename", "dedi-2", "device", "md2"))
	for dev, used := range map[string]float64{"nvme0": 115, "nvme1": 123} {
		d := []string{"node", "dedi-1", "device", dev}
		add("smartStatus", diskSeries(0, d...))
		add("smartCriticalWarning", diskSeries(4, d...))
		add("smartPercentageUsed", diskSeries(used, d...))
		add("smartSpare", diskSeries(100, d...))
		add("smartSpareThreshold", diskSeries(10, d...))
		add("smartMediaErrors", diskSeries(0, d...))
		add("smartExitStatus", diskSeries(8, d...))
		add("smartDevice", diskSeries(1, append(d, "model_name", "SAMSUNG MZVL2512HCJQ-00B00", "serial_number", "S-"+dev, "interface", "nvme")...))
	}
	add("smartDevices", diskSeries(2, "node", "dedi-1"))
	add("smartStatus", diskSeries(1, "node", "dedi-2", "device", "sda"))
	add("smartExitStatus", diskSeries(2, "node", "dedi-2", "device", "sdb"))
	add("smartDevices", diskSeries(2, "node", "dedi-2"))

	nodes := []nodeJSON{{Name: "dedi-1", Platform: "dedicated"}, {Name: "dedi-2", Platform: "dedicated"},
		{Name: "dedi-3", Platform: "dedicated"}, {Name: "cloud-1", Platform: "cloud"}}
	withDiskHealth(nodes, diskHealth(res))

	d1 := nodes[0].DiskHealth
	if d1 == nil || d1.Health != healthBad || d1.Summary != "2 disks failing" || !d1.SMART || len(d1.Arrays) != 2 || len(d1.Disks) != 2 {
		t.Fatalf("dedi-1: %+v", d1)
	}
	if a := d1.Arrays[0]; a.Device != "md0" || a.Health != healthOK || a.Active != 2 || a.Required != 2 || a.State != "active" {
		t.Errorf("md0: %+v", a)
	}
	nvme := d1.Disks[0]
	if nvme.Device != "nvme0" || nvme.Serial != "S-nvme0" || nvme.Model != "SAMSUNG MZVL2512HCJQ-00B00" || nvme.Health != healthBad || nvme.Unreadable ||
		!slices.Equal(nvme.Problems, []string{"SMART overall status: FAILED.", "Critical warning: reliability degraded.", "115 % of its rated endurance used."}) {
		t.Errorf("nvme0: %+v", nvme)
	}

	d2 := nodes[1].DiskHealth
	if d2 == nil || d2.Health != healthBad || d2.Summary != "RAID md2 degraded · 1 disk unreadable" {
		t.Fatalf("dedi-2: %+v", d2)
	}
	if a := d2.Arrays[0]; a.Problem != "Rebuilding, 43 % done; one disk short until then." {
		t.Errorf("md2: %+v", a)
	}
	if sda := d2.Disks[0]; sda.Health != healthOK || len(sda.Problems) != 0 {
		t.Errorf("sda: %+v", sda)
	}
	if sdb := d2.Disks[1]; sdb.Health != healthUnknown || !sdb.Unreadable {
		t.Errorf("sdb: %+v", sdb)
	}

	if d3 := nodes[2].DiskHealth; d3 == nil || d3.Health != healthUnknown || d3.Summary != "no SMART readings" || d3.SMART {
		t.Errorf("dedi-3: %+v", d3)
	}
	if nodes[3].DiskHealth != nil {
		t.Errorf("cloud-1: %+v", nodes[3].DiskHealth)
	}
}

func TestDiskVerdicts(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	worn := diskJSON{Device: "nvme0", PercentageUsed: f(81), MediaErrors: f(3), AvailableSpare: f(5), AvailableSpareThreshold: f(10)}
	diskVerdict(&worn)
	if worn.Health != healthBad || !slices.Equal(worn.Problems, []string{"Spare blocks at 5 %, below the drive's threshold of 10 %.",
		"81 % of its rated endurance used.", "3 media errors recorded."}) {
		t.Errorf("worn: %+v", worn)
	}
	fine := diskJSON{Device: "nvme1", PercentageUsed: f(80)}
	diskVerdict(&fine)
	if fine.Health != healthOK {
		t.Errorf("80 %% is not above 80: %+v", fine)
	}
	w := 0x41
	odd := diskJSON{Device: "nvme2", CriticalWarning: &w}
	diskVerdict(&odd)
	if !slices.Equal(odd.Problems, []string{"Critical warning: spare below threshold."}) {
		t.Errorf("odd: %+v", odd)
	}
	n := diskHealthJSON{Disks: []diskJSON{fine}, Arrays: []raidJSON{{Device: "md0", Health: healthOK}}, SMART: true}
	nodeVerdict(&n, true)
	if n.Health != healthOK || n.Summary != "1 disk healthy · 1 RAID array whole" {
		t.Errorf("node: %+v", n)
	}
	inactive := raidJSON{Device: "md3", State: "inactive", Required: 2}
	raidVerdict(&inactive)
	if inactive.Health != healthBad || inactive.Problem != "The array is inactive." {
		t.Errorf("inactive: %+v", inactive)
	}
}
