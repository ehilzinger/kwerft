// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The Kubernetes (k3s) upgrade preflight (docs/phase6-upgrades.md ›
// Kubernetes upgrade › Preflight). Nothing changes while it runs; the API
// (U5) runs it synchronously as UpgradeChecks.Kubernetes before it creates
// the Upgrade, and the controller runs it again.

// Preflight check names of a Kubernetes upgrade, besides CheckTarget,
// CheckNodesReady, CheckDiskSpace, CheckNoOtherOperation and
// CheckInstallerNode.
const (
	CheckNodesSameVersion  = "NodesSameVersion"
	CheckEtcd              = "Etcd"
	CheckDeprecatedAPIs    = "DeprecatedAPIs"
	CheckKwerftSupports    = "KwerftSupports"
	CheckUpgradeController = "UpgradeController"
)

// crdGVK reads CustomResourceDefinitions without apiextensions in the
// controllers' scheme.
var crdGVK = schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}

// Kubernetes runs the preflight of a Kubernetes upgrade to spec.version.
// self is the Upgrade being checked ("" before it exists).
func (c *UpgradeChecks) Kubernetes(ctx context.Context, spec kwerftv1.UpgradeSpec, self string) []kwerftv1.UpgradeCheck {
	var nodes corev1.NodeList
	nodesErr := c.Reader.List(ctx, &nodes)
	target, terr := upgrades.ParseVersion(spec.Version)
	if terr == nil && !upgrades.IsK3sVersion(spec.Version) {
		terr = fmt.Errorf("%s is not a k3s version (like v1.38.1+k3s1)", spec.Version)
	}
	var out []kwerftv1.UpgradeCheck
	out = append(out, k3sTargetCheck(target, terr, nodes.Items, nodesErr))
	out = append(out, c.nodesCheck(nodes.Items, nodesErr))
	out = append(out, sameVersionCheck(nodes.Items, nodesErr))
	out = append(out, c.etcdChecks(ctx, nodes.Items)...)
	if terr == nil && nodesErr == nil {
		_, oldest := kubeletVersions(nodes.Items)
		if from, err := upgrades.ParseVersion(oldest); err == nil {
			out = append(out, c.deprecatedAPIsCheck(ctx, from, target))
		}
	}
	out = append(out, k3sDiskCheck(nodes.Items, nodesErr))
	out = append(out, c.operationsCheck(ctx, self))
	if terr == nil {
		out = append(out, c.kwerftSupportsCheck(ctx, target))
	}
	out = append(out, c.upgradeControllerCheck(ctx))
	out = append(out, installerNodeCheck(nodes.Items))
	return out
}

// k3sTargetCheck: newer than every node, one minor at a time, never
// downward.
func k3sTargetCheck(target upgrades.Version, terr error, nodes []corev1.Node, nodesErr error) kwerftv1.UpgradeCheck {
	switch {
	case terr != nil:
		return check(CheckTarget, false, upperFirst(terr.Error())+".")
	case nodesErr != nil:
		return check(CheckTarget, false, "Cannot list the nodes: "+nodesErr.Error())
	}
	var oldest, newest *upgrades.Version
	for i := range nodes {
		v, err := upgrades.ParseVersion(nodes[i].Status.NodeInfo.KubeletVersion)
		if err != nil {
			continue
		}
		if oldest == nil || v.Less(*oldest) {
			oldest = &v
		}
		if newest == nil || newest.Less(v) {
			newest = &v
		}
	}
	switch {
	case oldest == nil:
		return check(CheckTarget, false, "No node reports its Kubernetes version.")
	case target.Compare(*newest) == 0 && target.Compare(*oldest) == 0:
		return check(CheckTarget, false, fmt.Sprintf("Every node already runs %s.", target))
	case target.Less(*newest) || target.Compare(*newest) == 0:
		return check(CheckTarget, false, fmt.Sprintf("Kubernetes cannot be downgraded: a node runs %s, not older than %s.", newest, target))
	case target.MinorsAhead(*oldest) > 1:
		return check(CheckTarget, false, fmt.Sprintf("Kubernetes moves one minor release at a time: from %s to %d.%d first, not %s.",
			oldest.MinorString(), oldest.Major, oldest.Minor+1, target.MinorString()))
	}
	return check(CheckTarget, true, fmt.Sprintf("%s → %s (%s).", oldest, target, upgrades.UpgradeKind(*oldest, target)))
}

func sameVersionCheck(nodes []corev1.Node, err error) kwerftv1.UpgradeCheck {
	if err != nil {
		return check(CheckNodesSameVersion, false, "Cannot list the nodes: "+err.Error())
	}
	byVersion := map[string][]string{}
	for i := range nodes {
		v := nodes[i].Status.NodeInfo.KubeletVersion
		byVersion[v] = append(byVersion[v], nodes[i].Name)
	}
	if len(byVersion) <= 1 {
		for v := range byVersion {
			return check(CheckNodesSameVersion, true, "Every node runs "+v+".")
		}
		return check(CheckNodesSameVersion, true, "No nodes.")
	}
	var parts []string
	for v, names := range byVersion {
		slices.Sort(names)
		parts = append(parts, v+": "+strings.Join(names, ", "))
	}
	slices.Sort(parts)
	return check(CheckNodesSameVersion, false, "The nodes run different versions ("+strings.Join(parts, "; ")+
		"). An unfinished upgrade? Bring every node to the same version first.")
}

// etcdMembers are the nodes with k3s's etcd role (else the control-plane
// nodes).
func etcdMembers(nodes []corev1.Node) []*corev1.Node {
	var etcd, servers []*corev1.Node
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[upgrades.LabelEtcd] == "true" {
			etcd = append(etcd, n)
		}
		if n.Labels[upgrades.LabelControlPlane] == "true" {
			servers = append(servers, n)
		}
	}
	if len(etcd) > 0 {
		return etcd
	}
	return servers
}

// etcdChecks: healthy (the API server's own check), and with quorum. With
// two members a restarting server takes quorum with it: a warning.
func (c *UpgradeChecks) etcdChecks(ctx context.Context, nodes []corev1.Node) []kwerftv1.UpgradeCheck {
	if c.APIServer == nil {
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, false, "The API server cannot be asked about etcd here.")}
	}
	if err := c.APIServer.EtcdReady(ctx); err != nil {
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, false, "etcd is not healthy: "+err.Error())}
	}
	members := etcdMembers(nodes)
	var down []string
	for _, n := range members {
		if !NodeReady(n) {
			down = append(down, n.Name)
		}
	}
	n, quorum := len(members), len(members)/2+1
	switch {
	case len(down) > 0 && n-len(down) < quorum:
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, false, fmt.Sprintf("etcd has lost quorum: %d of %d members are not ready (%s).", len(down), n, strings.Join(down, ", ")))}
	case len(down) > 0:
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, false, fmt.Sprintf("etcd members are not ready: %s. With one more down while a server restarts, quorum would be lost.", strings.Join(down, ", ")))}
	case n == 2:
		return []kwerftv1.UpgradeCheck{
			check(CheckEtcd, true, "etcd is healthy with 2 members."),
			{Check: CheckEtcd, Warning: true, Message: "With 2 etcd members, quorum is lost while either server restarts: the Kubernetes API is unavailable for that time. Three servers keep it."},
		}
	case n >= 3:
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, true, fmt.Sprintf("etcd is healthy with %d members; quorum holds while one server restarts.", n))}
	case n == 1:
		return []kwerftv1.UpgradeCheck{check(CheckEtcd, true, "etcd is healthy (one server: the Kubernetes API is unavailable while it restarts; workloads keep running).")}
	}
	return []kwerftv1.UpgradeCheck{check(CheckEtcd, true, "etcd is healthy.")}
}

// deprecatedAPIsCheck: deprecated APIs in use that the target removes
// block a minor upgrade and warn on a patch. The metric counts requests
// since the API server (the one answering) started.
func (c *UpgradeChecks) deprecatedAPIsCheck(ctx context.Context, from, target upgrades.Version) kwerftv1.UpgradeCheck {
	minor := !from.SameMinor(target)
	fail := func(msg string) kwerftv1.UpgradeCheck {
		return kwerftv1.UpgradeCheck{Check: CheckDeprecatedAPIs, Message: msg, Warning: !minor}
	}
	if c.APIServer == nil {
		return fail("The API server's metrics cannot be read here; check for APIs " + target.MinorString() + " removes yourself.")
	}
	apis, err := c.APIServer.DeprecatedAPIs(ctx)
	if err != nil {
		return fail("Cannot read the API server's metrics (" + err.Error() + "); check for APIs " + target.MinorString() + " removes yourself.")
	}
	var removed []string
	for _, a := range apis {
		if a.RemovedBy(target) {
			removed = append(removed, fmt.Sprintf("%s (removed in %s)", a, a.RemovedRelease))
		}
	}
	if len(removed) == 0 {
		return check(CheckDeprecatedAPIs, true, "No API that "+target.MinorString()+" removes was requested since the API server started.")
	}
	slices.Sort(removed)
	msg := "Requested since the API server started, and gone in " + target.MinorString() + ": " + strings.Join(removed, ", ") + "."
	if minor {
		msg += " Move their clients and manifests to the replacement APIs first."
	}
	return fail(msg)
}

func k3sDiskCheck(nodes []corev1.Node, err error) kwerftv1.UpgradeCheck {
	if err != nil {
		return check(CheckDiskSpace, false, "Cannot list the nodes: "+err.Error())
	}
	var short []string
	for i := range nodes {
		for _, cond := range nodes[i].Status.Conditions {
			if cond.Type == corev1.NodeDiskPressure && cond.Status == corev1.ConditionTrue {
				short = append(short, nodes[i].Name)
			}
		}
	}
	if len(short) > 0 {
		return check(CheckDiskSpace, false, "Short of disk space: "+strings.Join(short, ", ")+". Each node downloads the new k3s.")
	}
	return check(CheckDiskSpace, true, fmt.Sprintf("No node is short of disk space (the etcd snapshot needs %d GiB on the installer node, checked before it is taken).",
		upgrades.MinFreeBytesKubernetes>>30))
}

// kwerftSupportsCheck: the running Kwerft release supports the target
// (its manifest's kubernetes.supported).
func (c *UpgradeChecks) kwerftSupportsCheck(ctx context.Context, target upgrades.Version) kwerftv1.UpgradeCheck {
	if !upgrades.IsRelease(c.Version) {
		return kwerftv1.UpgradeCheck{Check: CheckKwerftSupports, Warning: true,
			Message: "Development build " + c.Version + ": which Kubernetes versions it supports is not known."}
	}
	if c.Releases == nil {
		return check(CheckKwerftSupports, false, "Release discovery is not available.")
	}
	m, err := c.Releases.Manifest(ctx, c.Version)
	if err != nil {
		return check(CheckKwerftSupports, false, "Cannot read the manifest of Kwerft "+c.Version+" ("+err.Error()+"): upgrade Kwerft first.")
	}
	if !m.Supports(target) {
		return check(CheckKwerftSupports, false, fmt.Sprintf("Kwerft %s supports Kubernetes %s, not %s: upgrade Kwerft first.",
			c.Version, strings.Join(m.Kubernetes.Supported, " and "), target.MinorString()))
	}
	return check(CheckKwerftSupports, true, fmt.Sprintf("Kwerft %s supports Kubernetes %s.", c.Version, target.MinorString()))
}

// upgradeControllerCheck: system-upgrade-controller (installer stage
// "upgrades", U2) is ready and its Plan CRD established.
func (c *UpgradeChecks) upgradeControllerCheck(ctx context.Context) kwerftv1.UpgradeCheck {
	const rerun = " Re-run the installer of the running release once on the installer node."
	crd := &unstructured.Unstructured{}
	crd.SetGroupVersionKind(crdGVK)
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: "plans." + upgrades.PlanGVK.Group}, crd); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return check(CheckUpgradeController, false, "system-upgrade-controller is not installed (no Plan CRD)."+rerun)
		}
		return check(CheckUpgradeController, false, "Cannot read the Plan CRD: "+err.Error())
	}
	if !crdEstablished(crd) {
		return check(CheckUpgradeController, false, "The Plan CRD is not established.")
	}
	var d appsv1.Deployment
	if err := c.Reader.Get(ctx, client.ObjectKey{Namespace: upgrades.SUCNamespace, Name: upgrades.SUCDeployment}, &d); err != nil {
		if apierrors.IsNotFound(err) {
			return check(CheckUpgradeController, false, "system-upgrade-controller is not installed (namespace "+upgrades.SUCNamespace+")."+rerun)
		}
		return check(CheckUpgradeController, false, "Cannot read system-upgrade-controller: "+err.Error())
	}
	if p := deploymentProblem(&d); p != "" {
		return check(CheckUpgradeController, false, "system-upgrade-controller "+p+".")
	}
	return check(CheckUpgradeController, true, "system-upgrade-controller is ready.")
}

func crdEstablished(crd *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(crd.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "Established" && m["status"] == "True" {
			return true
		}
	}
	return false
}

// deploymentProblem is "" for a rolled-out Deployment with every replica
// available, otherwise what is wrong.
func deploymentProblem(d *appsv1.Deployment) string {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	s := d.Status
	if s.ObservedGeneration < d.Generation || s.UpdatedReplicas < want || s.AvailableReplicas < want {
		return fmt.Sprintf("is not ready (%d of %d available)", min(s.UpdatedReplicas, s.AvailableReplicas), want)
	}
	return ""
}

// daemonSetProblem is deploymentProblem for DaemonSets.
func daemonSetProblem(ds *appsv1.DaemonSet) string {
	s := ds.Status
	if s.ObservedGeneration < ds.Generation || s.UpdatedNumberScheduled < s.DesiredNumberScheduled || s.NumberAvailable < s.DesiredNumberScheduled {
		return fmt.Sprintf("is not ready (%d of %d available)", min(s.UpdatedNumberScheduled, s.NumberAvailable), s.DesiredNumberScheduled)
	}
	return ""
}
