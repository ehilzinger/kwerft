// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/metrics"
)

// Disk health per node for Clusters › Nodes: software RAID (md) arrays from
// node-exporter, and on dedicated servers every disk's SMART readings from
// the chart's smartctl_exporter (templates/disk-health.yaml). The same
// series drive the alerts RAIDDegraded, DiskFailing, DiskWearing and
// DiskReadingsMissing (internal/alerting); this view says what they see.

// diskHealthTimeout bounds the VictoriaMetrics queries; the node list works
// without them.
const diskHealthTimeout = 5 * time.Second

// Health of a disk, an array or a node.
const (
	healthOK      = "ok"
	healthWarn    = "warn"    // worn, media errors, rebuilding
	healthBad     = "bad"     // failing disk, degraded or inactive array
	healthUnknown = "unknown" // no readings where Kwerft expects them
)

type raidJSON struct {
	Device   string `json:"device"`
	State    string `json:"state"` // active | inactive | recovering | resync | check
	Active   int    `json:"active"`
	Required int    `json:"required"`
	Failed   int    `json:"failed"`
	Spare    int    `json:"spare"`
	// SyncedPercent while the array rebuilds or resyncs.
	SyncedPercent *float64 `json:"syncedPercent,omitempty"`
	Health        string   `json:"health"`
	Problem       string   `json:"problem,omitempty"`
}

type diskJSON struct {
	Device    string `json:"device"`
	Model     string `json:"model,omitempty"`
	Serial    string `json:"serial,omitempty"`
	Interface string `json:"interface,omitempty"`
	// SMARTPassed is the drive's overall verdict; nil when it gives none.
	SMARTPassed *bool `json:"smartPassed,omitempty"`
	// CriticalWarning is the NVMe critical warning byte.
	CriticalWarning *int `json:"criticalWarning,omitempty"`
	// PercentageUsed of the rated endurance (NVMe); may pass 100.
	PercentageUsed          *float64 `json:"percentageUsed,omitempty"`
	AvailableSpare          *float64 `json:"availableSpare,omitempty"`
	AvailableSpareThreshold *float64 `json:"availableSpareThreshold,omitempty"`
	MediaErrors             *float64 `json:"mediaErrors,omitempty"`
	// Unreadable: smartctl could not read the disk (exit status bits 0-2).
	Unreadable bool     `json:"unreadable,omitempty"`
	Health     string   `json:"health"`
	Problems   []string `json:"problems"`
}

type diskHealthJSON struct {
	Health string `json:"health"`
	// Summary in a few words: "2 disks failing · RAID md1 degraded".
	Summary string `json:"summary"`
	// SMART: the node's smartctl_exporter answers and found disks
	// (dedicated servers).
	SMART  bool       `json:"smart"`
	Arrays []raidJSON `json:"arrays"`
	Disks  []diskJSON `json:"disks"`
}

// diskQueries are the instant queries behind diskHealth.
var diskQueries = []query{
	{key: "raidDisks", expr: metrics.RAIDDisks, instant: true},
	{key: "raidRequired", expr: metrics.RAIDRequired, instant: true},
	{key: "raidState", expr: metrics.RAIDState, instant: true},
	{key: "raidSynced", expr: metrics.RAIDSynced, instant: true},
	{key: "smartStatus", expr: metrics.SMARTStatus, instant: true},
	{key: "smartCriticalWarning", expr: metrics.SMARTCriticalWarning, instant: true},
	{key: "smartPercentageUsed", expr: metrics.SMARTPercentageUsed, instant: true},
	{key: "smartSpare", expr: metrics.SMARTSpare, instant: true},
	{key: "smartSpareThreshold", expr: metrics.SMARTSpareThreshold, instant: true},
	{key: "smartMediaErrors", expr: metrics.SMARTMediaErrors, instant: true},
	{key: "smartExitStatus", expr: metrics.SMARTExitStatus, instant: true},
	{key: "smartDevice", expr: metrics.SMARTDevice, instant: true},
	{key: "smartDevices", expr: metrics.SMARTDevices, instant: true},
}

// queryDiskHealth reads every node's disk health from the VictoriaMetrics
// of the cluster in ctx; false when it does not answer in time.
func (a *api) queryDiskHealth(ctx context.Context) (map[string]*diskHealthJSON, bool) {
	if a.metricsClient(ctx) == nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, diskHealthTimeout)
	defer cancel()
	rng, err := metrics.NewRange(a.now(), time.Hour, 0)
	if err != nil {
		return nil, false
	}
	res, err := a.runQueries(ctx, diskQueries, rng, metrics.Unconfined())
	if err != nil {
		a.cfg.Logger.Debug("nodes: disk health left out", "err", err)
		return nil, false
	}
	return diskHealth(res), true
}

// wearWarnPercent is where a disk counts as worn: the default of the
// DiskWearing alert.
var wearWarnPercent = func() float64 {
	info, _ := alerting.Lookup(kwerftv1.AlertDiskWearing)
	return float64(info.Threshold.Default)
}()

// diskHealth assembles the query results per node; withDiskHealth judges
// each node.
func diskHealth(res map[string][]metrics.Series) map[string]*diskHealthJSON {
	nodes := map[string]*diskHealthJSON{}
	node := func(name string) *diskHealthJSON {
		n := nodes[name]
		if n == nil {
			n = &diskHealthJSON{Arrays: []raidJSON{}, Disks: []diskJSON{}}
			nodes[name] = n
		}
		return n
	}

	// RAID arrays, by node and device.
	arrays := map[[2]string]*raidJSON{}
	array := func(s metrics.Series) *raidJSON {
		name, dev := s.Labels["nodename"], s.Labels["device"]
		if name == "" || dev == "" {
			return nil
		}
		k := [2]string{name, dev}
		if arrays[k] == nil {
			arrays[k] = &raidJSON{Device: dev}
		}
		return arrays[k]
	}
	for _, s := range res["raidDisks"] {
		if r := array(s); r != nil {
			n := int(value(s))
			switch s.Labels["state"] {
			case "active":
				r.Active = n
			case "failed":
				r.Failed = n
			case "spare":
				r.Spare = n
			}
		}
	}
	for _, s := range res["raidRequired"] {
		if r := array(s); r != nil {
			r.Required = int(value(s))
		}
	}
	for _, s := range res["raidState"] {
		if r := array(s); r != nil {
			r.State = s.Labels["state"]
		}
	}
	for _, s := range res["raidSynced"] {
		if r := array(s); r != nil {
			v := value(s)
			r.SyncedPercent = &v
		}
	}
	for k, r := range arrays {
		raidVerdict(r)
		n := node(k[0])
		n.Arrays = append(n.Arrays, *r)
	}

	// Disks, by node and device.
	disks := map[[2]string]*diskJSON{}
	disk := func(s metrics.Series) *diskJSON {
		name, dev := s.Labels["node"], s.Labels["device"]
		if name == "" || dev == "" {
			return nil
		}
		k := [2]string{name, dev}
		if disks[k] == nil {
			disks[k] = &diskJSON{Device: dev}
		}
		return disks[k]
	}
	num := func(key string, set func(*diskJSON, float64)) {
		for _, s := range res[key] {
			if d := disk(s); d != nil {
				set(d, value(s))
			}
		}
	}
	num("smartStatus", func(d *diskJSON, v float64) { ok := v == 1; d.SMARTPassed = &ok })
	num("smartCriticalWarning", func(d *diskJSON, v float64) { w := int(v); d.CriticalWarning = &w })
	num("smartPercentageUsed", func(d *diskJSON, v float64) { d.PercentageUsed = &v })
	num("smartSpare", func(d *diskJSON, v float64) { d.AvailableSpare = &v })
	num("smartSpareThreshold", func(d *diskJSON, v float64) { d.AvailableSpareThreshold = &v })
	num("smartMediaErrors", func(d *diskJSON, v float64) { d.MediaErrors = &v })
	num("smartExitStatus", func(d *diskJSON, v float64) { d.Unreadable = int(v)&7 != 0 })
	for _, s := range res["smartDevice"] {
		if d := disk(s); d != nil {
			d.Model, d.Serial, d.Interface = s.Labels["model_name"], s.Labels["serial_number"], s.Labels["interface"]
		}
	}
	for k, d := range disks {
		diskVerdict(d)
		n := node(k[0])
		n.Disks = append(n.Disks, *d)
	}
	for _, s := range res["smartDevices"] {
		if name := s.Labels["node"]; name != "" && value(s) > 0 {
			node(name).SMART = true
		}
	}

	for _, n := range nodes {
		slices.SortFunc(n.Arrays, func(x, y raidJSON) int { return strings.Compare(x.Device, y.Device) })
		slices.SortFunc(n.Disks, func(x, y diskJSON) int { return strings.Compare(x.Device, y.Device) })
	}
	return nodes
}

// raidVerdict judges an array the way RAIDDegraded does.
func raidVerdict(r *raidJSON) {
	r.State = cmp.Or(r.State, "active")
	r.Health = healthOK
	missing := r.Required - r.Active
	switch {
	case r.State == "inactive":
		r.Health, r.Problem = healthBad, "The array is inactive."
	case missing > 0 && (r.State == "recovering" || r.State == "resync") && r.SyncedPercent != nil:
		r.Health, r.Problem = healthBad, fmt.Sprintf("Rebuilding, %.0f %% done; %s until then.", *r.SyncedPercent, plural(missing, "one disk short", "%d disks short"))
	case missing > 0:
		r.Health, r.Problem = healthBad, fmt.Sprintf("Degraded: %d of %d disks active.", r.Active, r.Required)
	case r.Failed > 0:
		r.Health, r.Problem = healthBad, plural(r.Failed, "A disk is marked failed.", "%d disks are marked failed.")
	case r.State == "resync" || r.State == "recovering":
		r.Health, r.Problem = healthWarn, "Resyncing."
		if r.SyncedPercent != nil {
			r.Problem = fmt.Sprintf("Resyncing, %.0f %% done.", *r.SyncedPercent)
		}
	}
}

// criticalWarnings names the NVMe critical warning bits.
var criticalWarnings = []string{
	"spare below threshold", "temperature out of range", "reliability degraded", "read-only",
	"volatile memory backup failed", "persistent memory read-only",
}

// diskVerdict judges a disk the way DiskFailing and DiskWearing do; media
// errors only warn here, as the alert fires only on new ones.
func diskVerdict(d *diskJSON) {
	d.Health, d.Problems = healthOK, []string{}
	bad := func(p string) { d.Health = healthBad; d.Problems = append(d.Problems, p) }
	warn := func(p string) {
		if d.Health == healthOK || d.Health == healthUnknown {
			d.Health = healthWarn
		}
		d.Problems = append(d.Problems, p)
	}
	if d.Unreadable {
		d.Health = healthUnknown
		d.Problems = append(d.Problems, "smartctl cannot read this disk.")
	}
	if d.SMARTPassed != nil && !*d.SMARTPassed {
		bad("SMART overall status: FAILED.")
	}
	if w := ptrOr(d.CriticalWarning, 0); w != 0 {
		var names []string
		for i, name := range criticalWarnings {
			if w&(1<<i) != 0 {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			names = append(names, "0x"+strconv.FormatInt(int64(w), 16))
		}
		bad("Critical warning: " + strings.Join(names, ", ") + ".")
	}
	if d.AvailableSpare != nil && d.AvailableSpareThreshold != nil && *d.AvailableSpare < *d.AvailableSpareThreshold {
		bad(fmt.Sprintf("Spare blocks at %.0f %%, below the drive's threshold of %.0f %%.", *d.AvailableSpare, *d.AvailableSpareThreshold))
	}
	if u := ptrOr(d.PercentageUsed, 0); u > wearWarnPercent {
		warn(fmt.Sprintf("%.0f %% of its rated endurance used.", u))
	}
	if m := ptrOr(d.MediaErrors, 0); m > 0 {
		warn(plural(int(m), "1 media error recorded.", "%d media errors recorded."))
	}
}

// nodeVerdict sums up a node: the worst of its arrays and disks, unknown
// when a dedicated server sends no SMART readings.
func nodeVerdict(n *diskHealthJSON, dedicated bool) {
	rank := map[string]int{healthOK: 0, healthUnknown: 1, healthWarn: 2, healthBad: 3}
	n.Health = healthOK
	worse := func(h string) {
		if rank[h] > rank[n.Health] {
			n.Health = h
		}
	}
	var parts []string
	var failing, worn, unreadable int
	for _, d := range n.Disks {
		worse(d.Health)
		switch d.Health {
		case healthBad:
			failing++
		case healthWarn:
			worn++
		case healthUnknown:
			unreadable++
		}
	}
	for _, r := range n.Arrays {
		worse(r.Health)
		switch r.Health {
		case healthBad:
			parts = append(parts, "RAID "+r.Device+" degraded")
		case healthWarn:
			parts = append(parts, "RAID "+r.Device+" resyncing")
		}
	}
	if failing > 0 {
		parts = append(parts, plural(failing, "1 disk failing", "%d disks failing"))
	}
	if worn > 0 {
		parts = append(parts, plural(worn, "1 disk to watch", "%d disks to watch"))
	}
	if unreadable > 0 {
		parts = append(parts, plural(unreadable, "1 disk unreadable", "%d disks unreadable"))
	}
	if dedicated && !n.SMART {
		worse(healthUnknown)
		parts = append(parts, "no SMART readings")
	}
	if len(parts) == 0 {
		switch {
		case len(n.Disks) > 0 && len(n.Arrays) > 0:
			parts = append(parts, plural(len(n.Disks), "1 disk", "%d disks")+" healthy · "+plural(len(n.Arrays), "1 RAID array", "%d RAID arrays")+" whole")
		case len(n.Disks) > 0:
			parts = append(parts, plural(len(n.Disks), "1 disk", "%d disks")+" healthy")
		default:
			parts = append(parts, plural(len(n.Arrays), "1 RAID array", "%d RAID arrays")+" whole")
		}
	}
	n.Summary = strings.Join(parts, " · ")
}

// plural picks one or many; many may hold one %d for n.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	if strings.Contains(many, "%d") {
		return fmt.Sprintf(many, n)
	}
	return many
}

func ptrOr[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}

// withDiskHealth adds disk health to the nodes that have any, and to every
// dedicated server (which should report SMART readings).
func withDiskHealth(nodes []nodeJSON, health map[string]*diskHealthJSON) {
	for i := range nodes {
		n := &nodes[i]
		dedicated := n.Platform == "dedicated"
		h := health[n.Name]
		if h == nil {
			if !dedicated {
				continue
			}
			h = &diskHealthJSON{Arrays: []raidJSON{}, Disks: []diskJSON{}}
		}
		nodeVerdict(h, dedicated)
		n.DiskHealth = h
	}
}
