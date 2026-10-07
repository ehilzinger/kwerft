// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Preflight check names (Upgrade.status.preflight[].check).
const (
	CheckTarget           = "Target"
	CheckImagePullable    = "ImagePullable"
	CheckNodesReady       = "NodesReady"
	CheckDiskSpace        = "DiskSpace"
	CheckNoOtherOperation = "NoOtherOperation"
	CheckReleaseInstall   = "ReleaseInstall"
	CheckInstallerNode    = "InstallerNode"
	CheckAgentSkew        = "AgentSkew"
	CheckDataRollback     = "DataRollback"
)

// DefaultImageRepository is the released console image (install.sh
// KWERFT_IMAGE_REPO); a console running anything else is a --image dev
// install.
const DefaultImageRepository = "ghcr.io/ehilzinger/kwerft"

// SelfImage is the console's own container: its image as the Deployment
// names it, and the image ID the node pulled (repository@sha256:…).
type SelfImage struct {
	Image   string
	ImageID string
}

// UpgradeChecks is the Kwerft upgrade preflight (docs/phase6-upgrades.md ›
// Kwerft upgrade › Preflight). Nothing changes while it runs. The Upgrade
// controller runs it; the API (U5) runs the same checks synchronously
// before it creates an Upgrade.
type UpgradeChecks struct {
	// Reader reads nodes, Upgrades, NodePools, Clusters and ConsoleSettings.
	Reader client.Reader
	// Releases reads the install repository.
	Releases upgrades.Source
	// Registry checks the image and chart are pullable anonymously.
	Registry upgrades.Registry
	// Version is the running Kwerft release.
	Version string
	// Self returns the console's own image; nil skips the dev-install check
	// on the image (the version is still checked).
	Self func(ctx context.Context) (SelfImage, error)
	// ImageRepository defaults to DefaultImageRepository.
	ImageRepository string
	// DataDir is the console's data volume; empty skips its disk check
	// (agent mode has none).
	DataDir string
	// FreeBytes reads free space of a local path (statfs).
	FreeBytes func(path string) (uint64, error)
	// Cluster is this cluster's name for NodePools ("local" for the
	// console's).
	Cluster string
	// APIServer answers the Kubernetes preflight's etcd and deprecated-API
	// questions (k3s_preflight.go); nil fails those checks.
	APIServer upgrades.APIServerInfo
	// NodePools reads the NodePools, which live in the management cluster;
	// nil reads them with Reader (the console's own cluster).
	NodePools client.Reader
	// FollowsConsole: an agent cluster, which takes its console's release
	// (the console never offers one past its own): the channel is not
	// checked (an agent has no update settings), agent skew is the
	// console's check, and there is no database to roll back.
	FollowsConsole bool
}

// Facts the checks gathered, for the controller.
type upgradeFacts struct {
	manifest   *upgrades.Manifest
	kubernetes string // oldest kubelet
}

func check(name string, ok bool, msg string) kwerftv1.UpgradeCheck {
	return kwerftv1.UpgradeCheck{Check: name, OK: ok, Message: msg}
}

// Blocked reports whether a failed check blocks the upgrade (warnings do
// not).
func Blocked(checks []kwerftv1.UpgradeCheck) []kwerftv1.UpgradeCheck {
	var out []kwerftv1.UpgradeCheck
	for _, c := range checks {
		if !c.OK && !c.Warning {
			out = append(out, c)
		}
	}
	return out
}

// Kwerft runs the preflight of a Kwerft upgrade to spec.version. self is
// the Upgrade being checked ("" before it exists), so it does not count as
// another operation.
func (c *UpgradeChecks) Kwerft(ctx context.Context, spec kwerftv1.UpgradeSpec, self string) []kwerftv1.UpgradeCheck {
	checks, _ := c.kwerft(ctx, spec, self)
	return checks
}

func (c *UpgradeChecks) kwerft(ctx context.Context, spec kwerftv1.UpgradeSpec, self string) ([]kwerftv1.UpgradeCheck, upgradeFacts) {
	var facts upgradeFacts
	var nodes corev1.NodeList
	nodesErr := c.Reader.List(ctx, &nodes)
	minors, oldest := kubeletVersions(nodes.Items)
	facts.kubernetes = oldest

	// The target: newer, on the channel, allowed by the manifest's rules.
	target, terr := upgrades.ParseVersion(spec.Version)
	cur, cerr := upgrades.ParseVersion(c.Version)
	var out []kwerftv1.UpgradeCheck
	switch {
	case terr != nil:
		out = append(out, check(CheckTarget, false, terr.Error()))
	case cerr != nil:
		out = append(out, check(CheckTarget, false, fmt.Sprintf("The running version %q is not a release.", c.Version)))
	case !cur.Less(target):
		out = append(out, check(CheckTarget, false, fmt.Sprintf("%s is not newer than the running %s.", spec.Version, c.Version)))
	case nodesErr != nil:
		out = append(out, check(CheckTarget, false, "Cannot list the nodes: "+nodesErr.Error()))
	default:
		out = append(out, c.targetCheck(ctx, target, cur, minors, &facts))
	}

	// Image and chart, anonymously (the installer's exit-50 check).
	switch m := facts.manifest; {
	case m == nil:
		out = append(out, check(CheckImagePullable, false, "Unknown until the target's manifest is read."))
	case m.Image == "" || m.Chart.Ref == "" || m.Chart.Version == "":
		out = append(out, check(CheckImagePullable, false, "The manifest of "+m.Version+" names no image or chart."))
	default:
		chart := m.Chart.Ref + ":" + m.Chart.Version
		var problems []string
		for _, ref := range []string{m.Image, chart} {
			if err := c.Registry.Pullable(ctx, ref); err != nil {
				problems = append(problems, err.Error())
			}
		}
		if len(problems) > 0 {
			out = append(out, check(CheckImagePullable, false, strings.Join(problems, "; ")+"."))
		} else {
			out = append(out, check(CheckImagePullable, true, "Image and chart of "+m.Version+" can be pulled."))
		}
	}

	out = append(out, c.nodesCheck(nodes.Items, nodesErr))
	out = append(out, c.diskCheck(nodes.Items))
	out = append(out, c.operationsCheck(ctx, self))
	out = append(out, c.releaseInstallCheck(ctx))
	out = append(out, installerNodeCheck(nodes.Items))
	if terr == nil && !c.FollowsConsole {
		out = append(out, c.agentSkewChecks(ctx, target)...)
	}
	switch m := facts.manifest; {
	case m == nil:
	case c.FollowsConsole:
		out = append(out, check(CheckDataRollback, true, "An agent keeps no database: a rollback loses nothing."))
	case !m.IsRollbackSafe() && !spec.AcceptDataRollback:
		out = append(out, check(CheckDataRollback, false, m.Version+" is not rollback-safe: a rollback restores the database copy and loses what changed since. Accept that to continue."))
	case !m.IsRollbackSafe():
		out = append(out, check(CheckDataRollback, true, "Accepted: a rollback restores the database copy."))
	default:
		out = append(out, check(CheckDataRollback, true, m.Version+" can be rolled back without losing data."))
	}
	return out, facts
}

func (c *UpgradeChecks) channel(ctx context.Context) string {
	var s kwerftv1.ConsoleSettings
	if err := c.Reader.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err == nil &&
		s.Spec.Updates != nil && s.Spec.Updates.Channel != "" {
		return s.Spec.Updates.Channel
	}
	return upgrades.ChannelStable
}

func (c *UpgradeChecks) targetCheck(ctx context.Context, target, cur upgrades.Version, minors []upgrades.Version, facts *upgradeFacts) kwerftv1.UpgradeCheck {
	if c.Releases == nil {
		return check(CheckTarget, false, "Release discovery is not available.")
	}
	releases, err := c.Releases.Releases(ctx)
	if err != nil {
		return check(CheckTarget, false, "Cannot read the releases: "+err.Error())
	}
	channel := c.channel(ctx)
	i := slices.IndexFunc(releases, func(r upgrades.Release) bool {
		v, err := upgrades.ParseVersion(r.Version)
		return err == nil && v.Compare(target) == 0
	})
	switch {
	case i < 0:
		return check(CheckTarget, false, target.String()+" is not a published release.")
	case !c.FollowsConsole && !upgrades.OnChannel(releases[i].Channel, channel):
		return check(CheckTarget, false, fmt.Sprintf("%s is on the %s channel; this console follows %s.", target, releases[i].Channel, channel))
	}
	m, err := c.Releases.Manifest(ctx, target.String())
	if err != nil {
		return check(CheckTarget, false, "Cannot read the manifest of "+target.String()+": "+err.Error())
	}
	facts.manifest = m
	if ok, reason := upgrades.KwerftAllowed(m, cur, minors); !ok {
		return check(CheckTarget, false, reason)
	}
	return check(CheckTarget, true, fmt.Sprintf("%s → %s (%s).", cur, target, upgrades.UpgradeKind(cur, target)))
}

// kubeletVersions returns the k3s minors the nodes run and the oldest
// version.
func kubeletVersions(nodes []corev1.Node) ([]upgrades.Version, string) {
	var minors []upgrades.Version
	var oldest *upgrades.Version
	for _, n := range nodes {
		v, err := upgrades.ParseVersion(n.Status.NodeInfo.KubeletVersion)
		if err != nil {
			continue
		}
		if !slices.ContainsFunc(minors, v.SameMinor) {
			minors = append(minors, v)
		}
		if oldest == nil || v.Less(*oldest) {
			oldest = &v
		}
	}
	if oldest == nil {
		return minors, ""
	}
	return minors, oldest.String()
}

func (c *UpgradeChecks) nodesCheck(nodes []corev1.Node, err error) kwerftv1.UpgradeCheck {
	if err != nil {
		return check(CheckNodesReady, false, "Cannot list the nodes: "+err.Error())
	}
	var notReady []string
	for i := range nodes {
		if !NodeReady(&nodes[i]) {
			notReady = append(notReady, nodes[i].Name)
		}
	}
	if len(notReady) > 0 {
		return check(CheckNodesReady, false, "Not ready: "+strings.Join(notReady, ", ")+".")
	}
	return check(CheckNodesReady, true, fmt.Sprintf("All %d nodes are ready.", len(nodes)))
}

func (c *UpgradeChecks) diskCheck(nodes []corev1.Node) kwerftv1.UpgradeCheck {
	var problems []string
	for i := range nodes {
		n := &nodes[i]
		if n.Labels[kwerftv1.LabelInstaller] != "true" {
			continue
		}
		for _, cond := range n.Status.Conditions {
			if cond.Type == corev1.NodeDiskPressure && cond.Status == corev1.ConditionTrue {
				problems = append(problems, "the installer node "+n.Name+" is short of disk space")
			}
		}
	}
	if c.DataDir != "" && c.FreeBytes != nil {
		free, err := c.FreeBytes(c.DataDir)
		switch {
		case err != nil:
			problems = append(problems, "the console's data volume: "+err.Error())
		case free < upgrades.MinFreeBytes:
			problems = append(problems, fmt.Sprintf("the console's data volume has %.1f GiB free, %d GiB needed", float64(free)/(1<<30), upgrades.MinFreeBytes>>30))
		}
	}
	if len(problems) > 0 {
		return check(CheckDiskSpace, false, upperFirst(strings.Join(problems, "; "))+".")
	}
	return check(CheckDiskSpace, true, fmt.Sprintf("Enough free space (the runner checks for %d GiB on the installer node before it starts).", upgrades.MinFreeBytes>>30))
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// poolBusy: a node of the pool is being created, joined, drained or
// deleted.
func poolBusy(p *kwerftv1.NodePool) bool {
	for _, n := range p.Status.Nodes {
		switch n.Phase {
		case PoolNodeCreating, PoolNodeJoining, PoolNodeDraining, PoolNodeDeleting:
			return true
		}
	}
	return false
}

func (c *UpgradeChecks) operationsCheck(ctx context.Context, self string) kwerftv1.UpgradeCheck {
	var busy []string
	var ups kwerftv1.UpgradeList
	if err := c.Reader.List(ctx, &ups); err != nil {
		return check(CheckNoOtherOperation, false, "Cannot list Upgrades: "+err.Error())
	}
	for _, u := range ups.Items {
		if u.Name != self && upgrades.Active(u.Status.Phase) {
			busy = append(busy, "Upgrade "+u.Name+" is "+string(u.Status.Phase))
		}
	}
	var pools kwerftv1.NodePoolList
	poolReader := c.NodePools
	if poolReader == nil {
		poolReader = c.Reader
	}
	if err := poolReader.List(ctx, &pools); err != nil && !meta.IsNoMatchError(err) {
		return check(CheckNoOtherOperation, false, "Cannot list node pools: "+err.Error())
	}
	cluster := c.Cluster
	if cluster == "" {
		cluster = clusters.Local
	}
	for i := range pools.Items {
		if pools.Items[i].Spec.Cluster == cluster && poolBusy(&pools.Items[i]) {
			busy = append(busy, "node pool "+pools.Items[i].Name+" is changing nodes")
		}
	}
	if len(busy) > 0 {
		return check(CheckNoOtherOperation, false, upperFirst(strings.Join(busy, "; "))+".")
	}
	return check(CheckNoOtherOperation, true, "No other upgrade or node pool change is running.")
}

func (c *UpgradeChecks) releaseInstallCheck(ctx context.Context) kwerftv1.UpgradeCheck {
	const hint = " Development installs are upgraded with make dev-server (or re-run the installer without --image)."
	if !upgrades.IsRelease(c.Version) {
		return check(CheckReleaseInstall, false, "This console runs development build "+c.Version+"."+hint)
	}
	if c.Self == nil {
		return check(CheckReleaseInstall, true, "Release "+c.Version+".")
	}
	self, err := c.Self(ctx)
	if err != nil {
		return check(CheckReleaseInstall, false, "Cannot read the console's own image: "+err.Error())
	}
	repo := c.ImageRepository
	if repo == "" {
		repo = DefaultImageRepository
	}
	if want := repo + ":" + strings.TrimPrefix(c.Version, "v"); self.Image != want {
		return check(CheckReleaseInstall, false, "This console runs image "+self.Image+", not the release image "+want+" (an --image install)."+hint)
	}
	return check(CheckReleaseInstall, true, "Release image "+self.Image+".")
}

func installerNodeCheck(nodes []corev1.Node) kwerftv1.UpgradeCheck {
	for i := range nodes {
		if nodes[i].Labels[kwerftv1.LabelInstaller] == "true" {
			if !NodeReady(&nodes[i]) {
				return check(CheckInstallerNode, false, "The installer node "+nodes[i].Name+" is not ready.")
			}
			return check(CheckInstallerNode, true, "The installer runs on "+nodes[i].Name+".")
		}
	}
	// The installer labels its node and writes install.env (which the
	// upgrade's installer run needs) since Phase 6; the runner checks the
	// file itself before it changes anything.
	return check(CheckInstallerNode, false, "No node is labelled "+kwerftv1.LabelInstaller+"=true: this cluster was installed by a release "+
		"from before console upgrades (no "+upgrades.HostInstallEnv+"). Re-run the installer of the running release once on the server Kwerft was installed on.")
}

// agentSkewChecks: console N works with agents of the same minor and the
// one before; a console upgrade must not leave a connected agent further
// behind. Disconnected clusters are skipped with a warning.
func (c *UpgradeChecks) agentSkewChecks(ctx context.Context, target upgrades.Version) []kwerftv1.UpgradeCheck {
	var list kwerftv1.ClusterList
	if err := c.Reader.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return nil
		}
		return []kwerftv1.UpgradeCheck{check(CheckAgentSkew, false, "Cannot list clusters: "+err.Error())}
	}
	var behind, skipped []string
	for _, cl := range list.Items {
		if cl.Name == clusters.Local {
			continue
		}
		if cl.Status.Phase != ClusterConnected {
			skipped = append(skipped, cl.Name)
			continue
		}
		v, err := upgrades.ParseVersion(cl.Status.AgentVersion)
		if err != nil {
			skipped = append(skipped, cl.Name)
			continue
		}
		if target.MinorsAhead(v) > 1 {
			behind = append(behind, fmt.Sprintf("%s runs %s", cl.Name, cl.Status.AgentVersion))
		}
	}
	var out []kwerftv1.UpgradeCheck
	if len(behind) > 0 {
		out = append(out, check(CheckAgentSkew, false, fmt.Sprintf("Agents must be at most one minor behind %s: %s. Upgrade them first.", target, strings.Join(behind, ", "))))
	} else {
		out = append(out, check(CheckAgentSkew, true, "Every connected agent stays within one minor release."))
	}
	if len(skipped) > 0 {
		out = append(out, kwerftv1.UpgradeCheck{Check: CheckAgentSkew, Warning: true,
			Message: "Not connected or unknown version, skipped: " + strings.Join(skipped, ", ") + "."})
	}
	return out
}
