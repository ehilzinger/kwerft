// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Kubernetes (k3s) upgrades (docs/phase6-upgrades.md › Kubernetes upgrade)
// move through system-upgrade-controller (SUC), which the installer
// installs (U2, SYSTEM_UPGRADE_CONTROLLER_VERSION). The names below are
// SUC's upstream manifests' and its Plan API's (upgrade.cattle.io/v1,
// pkg/apis/upgrade.cattle.io at v0.20.2).
const (
	SUCNamespace      = "system-upgrade"
	SUCServiceAccount = "system-upgrade"
	SUCDeployment     = "system-upgrade-controller"
	// K3sUpgradeImage is the image SUC runs on each node; SUC appends
	// the Plan's version as the tag ("+" becomes "-").
	K3sUpgradeImage = "rancher/k3s-upgrade"

	// The Plans of one Kubernetes upgrade. PlanAgentCordon exists only
	// when a worker pool has a single node: it is cordoned, not drained.
	PlanServer      = "k3s-server"
	PlanAgent       = "k3s-agent"
	PlanAgentCordon = "k3s-agent-cordon"

	// SUC's labels on the Jobs it creates (one per node and Plan).
	SUCLabelPlan = "upgrade.cattle.io/plan"
	SUCLabelNode = "upgrade.cattle.io/node"

	// k3s's node role labels.
	LabelControlPlane = "node-role.kubernetes.io/control-plane"
	LabelEtcd         = "node-role.kubernetes.io/etcd"

	// MinFreeBytesKubernetes on the installer node for the etcd snapshot.
	MinFreeBytesKubernetes = 2 << 30

	// K3sSnapshotDir is where k3s keeps local etcd snapshots.
	K3sSnapshotDir = "/var/lib/rancher/k3s/server/db/snapshots"
)

// PlanGVK is SUC's Plan.
var PlanGVK = schema.GroupVersionKind{Group: "upgrade.cattle.io", Version: "v1", Kind: "Plan"}

// Plans are every Plan a Kubernetes upgrade may create.
var Plans = []string{PlanServer, PlanAgent, PlanAgentCordon}

// IsK3sVersion: a k3s release (v1.38.1+k3s1), not a Kwerft one.
func IsK3sVersion(v string) bool {
	pv, err := ParseVersion(v)
	return err == nil && strings.HasPrefix(v, "v") && strings.HasPrefix(pv.Build, "k3s")
}

// DeprecatedAPI is one series of the API server's
// apiserver_requested_deprecated_apis: a deprecated API that was requested
// since the API server started.
type DeprecatedAPI struct {
	Group, Version, Resource, Subresource string
	// RemovedRelease is "1.38" (empty: no removal planned).
	RemovedRelease string
}

func (d DeprecatedAPI) String() string {
	gv := d.Version
	if d.Group != "" {
		gv = d.Group + "/" + d.Version
	}
	r := d.Resource
	if d.Subresource != "" {
		r += "/" + d.Subresource
	}
	return gv + " " + r
}

// RemovedBy reports whether the API is gone in target (removed_release ≤
// the target's minor).
func (d DeprecatedAPI) RemovedBy(target Version) bool {
	if d.RemovedRelease == "" {
		return false
	}
	majS, minS, ok := strings.Cut(d.RemovedRelease, ".")
	if !ok {
		return false
	}
	ma, err1 := strconv.Atoi(majS)
	mi, err2 := strconv.Atoi(minS)
	if err1 != nil || err2 != nil {
		return false
	}
	return ma < target.Major || (ma == target.Major && mi <= target.Minor)
}

const deprecatedMetric = "apiserver_requested_deprecated_apis"

// ParseDeprecatedAPIs reads the series of apiserver_requested_deprecated_apis
// with value 1 from the API server's /metrics (Prometheus text format).
func ParseDeprecatedAPIs(metrics []byte) []DeprecatedAPI {
	var out []DeprecatedAPI
	sc := bufio.NewScanner(bytes.NewReader(metrics))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, deprecatedMetric+"{") {
			continue
		}
		end := strings.LastIndexByte(line, '}')
		if end < 0 {
			continue
		}
		value := strings.TrimSpace(line[end+1:])
		if f := strings.Fields(value); len(f) > 0 {
			value = f[0] // a timestamp may follow
		}
		if v, err := strconv.ParseFloat(value, 64); err != nil || v != 1 {
			continue
		}
		labels := parseLabels(line[len(deprecatedMetric)+1 : end])
		d := DeprecatedAPI{Group: labels["group"], Version: labels["version"], Resource: labels["resource"],
			Subresource: labels["subresource"], RemovedRelease: labels["removed_release"]}
		if !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// parseLabels reads name="value",… with Prometheus's escapes.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		eq := strings.IndexByte(s, '=')
		if eq < 0 || eq+1 >= len(s) || s[eq+1] != '"' {
			break
		}
		name := strings.TrimSpace(s[:eq])
		var val strings.Builder
		i := eq + 2
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(s[i])
				}
				continue
			}
			val.WriteByte(s[i])
		}
		out[name] = val.String()
		if i >= len(s) {
			break
		}
		s = s[i+1:]
	}
	return out
}

// APIServerInfo is what the Kubernetes preflight asks the API server.
type APIServerInfo interface {
	// EtcdReady is the API server's own etcd health check (/readyz/etcd).
	EtcdReady(ctx context.Context) error
	// DeprecatedAPIs requested since the API server started.
	DeprecatedAPIs(ctx context.Context) ([]DeprecatedAPI, error)
}

// APIServer asks the API server over its non-resource URLs (the
// controller's ClusterRole grants /metrics and /readyz/etcd).
type APIServer struct {
	REST rest.Interface
}

func (a *APIServer) EtcdReady(ctx context.Context) error {
	body, err := a.REST.Get().AbsPath("/readyz/etcd").DoRaw(ctx)
	if err != nil {
		if msg := strings.TrimSpace(string(body)); msg != "" {
			return fmt.Errorf("%s", truncate(msg, 200))
		}
		return err
	}
	return nil
}

func (a *APIServer) DeprecatedAPIs(ctx context.Context) ([]DeprecatedAPI, error) {
	body, err := a.REST.Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return nil, err
	}
	return ParseDeprecatedAPIs(body), nil
}

// RestoreHint is the end of a failed Kubernetes upgrade's message: k3s
// cannot be downgraded, and restoring the etcd snapshot resets the whole
// cluster to its moment, so Kwerft never does it; the full procedure is in
// docs/phase6-upgrades.md › Restoring after a failed Kubernetes upgrade.
func RestoreHint(snapshot, from string) string {
	var b strings.Builder
	b.WriteString("Kubernetes cannot be downgraded, and Kwerft does not restore etcd by itself.")
	if snapshot != "" {
		fmt.Fprintf(&b, " The etcd snapshot %s was taken before the upgrade (`k3s etcd-snapshot ls` on the installer node lists its file under %s).", snapshot, K3sSnapshotDir)
	}
	b.WriteString(" Nodes one minor behind keep working within the version skew policy: fix the cause and upgrade again.")
	b.WriteString(" To go back to the snapshot instead (this resets the whole cluster to that moment): stop k3s on every node,")
	if from != "" {
		fmt.Fprintf(&b, " put k3s %s back on every upgraded node (/usr/local/bin/k3s from that k3s release),", from)
	}
	b.WriteString(" run `k3s server --cluster-reset --cluster-reset-restore-path=<snapshot file>` on the installer node and start k3s there,")
	b.WriteString(" then delete /var/lib/rancher/k3s/server/db on the other servers and start k3s on every node")
	b.WriteString(" (docs/phase6-upgrades.md › Restoring after a failed Kubernetes upgrade).")
	return b.String()
}

// ReachedRunning reports whether an Upgrade got as far as Running, i.e.
// changed something: a Kwerft upgrade records the Helm revisions in the
// same write that moves it to Running (and the installer's steps after),
// a Kubernetes upgrade its nodes. A failed preflight or backup (nothing
// changed) did not.
func ReachedRunning(u *kwerftv1.Upgrade) bool {
	switch u.Status.Phase {
	case kwerftv1.UpgradeRunning, kwerftv1.UpgradeVerifying, kwerftv1.UpgradeRollingBack, kwerftv1.UpgradeRolledBack:
		return true
	}
	if u.Spec.Component == kwerftv1.UpgradeKubernetes {
		return len(u.Status.Nodes) > 0
	}
	return len(u.Status.Steps) > 0 || (u.Status.Backup != nil && u.Status.Backup.HelmRevisions != nil)
}
