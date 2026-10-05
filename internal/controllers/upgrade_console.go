package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The console API's preflight (server.UpgradePreflight) for every cluster
// it manages (docs/phase6-upgrades.md › As built (G1)): the local
// cluster's UpgradeChecks, and for a connected agent cluster the same
// checks with that cluster's reader (Kwerft's identity there, through the
// tunnel) and the agent's release. The Upgrade itself is created by the
// API as the user, in that cluster; the agent's own Upgrade controller runs
// the checks again.

// ErrUpgradeUnsupported: upgrades of the cluster cannot be started from
// the console (unknown or disconnected, or an agent without the Upgrade
// kind: a release from before console upgrades).
var ErrUpgradeUnsupported = errors.New("upgrades of this cluster cannot be started from the console")

// CheckAgentTarget: an agent is upgraded to its console's release at most,
// never past it.
const CheckAgentTarget = "AgentTarget"

// ConsolePreflight runs upgrade preflights for the console API.
type ConsolePreflight struct {
	// Local is the console's own cluster's preflight.
	Local *UpgradeChecks
	// Management reads the Cluster objects (agent versions) and NodePools.
	Management client.Reader
	// Clusters reaches agent clusters with Kwerft's identity there; nil:
	// the local cluster only.
	Clusters ClusterClients
	// APIServer answers an agent cluster's etcd and deprecated-API
	// questions (the Kubernetes preflight); nil fails those checks.
	APIServer func(cluster string) (upgrades.APIServerInfo, error)
	// Version is the console's running release.
	Version string
	// Now returns the current time; nil means time.Now.
	Now func() time.Time

	mu        sync.Mutex
	supported map[string]cachedAnswer[bool]
	manifests map[string]cachedAnswer[*upgrades.Manifest]
}

type cachedAnswer[T any] struct {
	v  T
	at time.Time
	ok bool // false: an error, kept for a shorter time
}

const (
	// supportedTTL: how long "this agent has the Upgrade kind" is trusted
	// (Settings › Updates asks on every refresh).
	supportedTTL = time.Minute
	// manifestTTL: a release's manifest does not change; failures are
	// asked again sooner.
	manifestTTL, manifestErrTTL = 6 * time.Hour, 10 * time.Minute
)

func (p *ConsolePreflight) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// agent is a connected agent cluster's Cluster object, or an error.
func (p *ConsolePreflight) agent(ctx context.Context, cluster string) (*kwerftv1.Cluster, error) {
	if p.Clusters == nil || p.Management == nil {
		return nil, ErrUpgradeUnsupported
	}
	var cl kwerftv1.Cluster
	if err := p.Management.Get(ctx, client.ObjectKey{Name: cluster}, &cl); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpgradeUnsupported, err)
	}
	if cl.Status.Phase != ClusterConnected {
		return nil, fmt.Errorf("%w: cluster %s is not connected", ErrUpgradeUnsupported, cluster)
	}
	return &cl, nil
}

// Supports: the local cluster; an agent cluster while it is connected and
// knows the Upgrade kind (agents from before console upgrades do not).
func (p *ConsolePreflight) Supports(cluster string) bool {
	if cluster == "" || cluster == clusters.Local {
		return p.Local != nil
	}
	p.mu.Lock()
	if a, ok := p.supported[cluster]; ok && p.now().Sub(a.at) < supportedTTL {
		p.mu.Unlock()
		return a.v
	}
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok := p.probe(ctx, cluster)
	p.mu.Lock()
	if p.supported == nil {
		p.supported = map[string]cachedAnswer[bool]{}
	}
	p.supported[cluster] = cachedAnswer[bool]{v: ok, at: p.now(), ok: true}
	p.mu.Unlock()
	return ok
}

func (p *ConsolePreflight) probe(ctx context.Context, cluster string) bool {
	if _, err := p.agent(ctx, cluster); err != nil {
		return false
	}
	c, err := p.Clusters.For(ctx, cluster)
	if err != nil {
		return false
	}
	var list kwerftv1.UpgradeList
	return c.List(ctx, &list, client.Limit(1)) == nil
}

// Preflight checks spec in the cluster (clusters.Local or an agent's).
func (p *ConsolePreflight) Preflight(ctx context.Context, cluster string, spec kwerftv1.UpgradeSpec) ([]kwerftv1.UpgradeCheck, error) {
	if cluster == "" || cluster == clusters.Local {
		if p.Local == nil {
			return nil, ErrUpgradeUnsupported
		}
		return p.run(ctx, p.Local, spec)
	}
	cl, err := p.agent(ctx, cluster)
	if err != nil {
		return nil, err
	}
	c, err := p.Clusters.For(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpgradeUnsupported, err)
	}
	var probe kwerftv1.UpgradeList
	if err := c.List(ctx, &probe, client.Limit(1)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("%w: its agent knows no upgrades", ErrUpgradeUnsupported)
		}
		return nil, err
	}
	agent := strings.TrimPrefix(cl.Status.AgentVersion, "v")
	checks := &UpgradeChecks{Reader: c, Version: agent, Cluster: cluster, NodePools: p.Management, FollowsConsole: true}
	if p.Local != nil {
		checks.Releases, checks.Registry, checks.ImageRepository = p.Local.Releases, p.Local.Registry, p.Local.ImageRepository
	}
	if spec.Component == kwerftv1.UpgradeKubernetes && p.APIServer != nil {
		if checks.APIServer, err = p.APIServer(cluster); err != nil {
			checks.APIServer = nil // the etcd and deprecated-API checks fail and say so
		}
	}
	out, err := p.run(ctx, checks, spec)
	if err != nil || spec.Component != kwerftv1.UpgradeKwerft {
		return out, err
	}
	// Never past the console: the console upgrades first, then its agents.
	at := check(CheckAgentTarget, true, fmt.Sprintf("The console runs %s; %s follows it.", p.Version, cluster))
	if err := upgrades.AgentTargetAllowed(p.Version, agent, spec.Version); err != nil {
		at = check(CheckAgentTarget, false, upperFirst(err.Error())+".")
	}
	return append([]kwerftv1.UpgradeCheck{at}, out...), nil
}

func (p *ConsolePreflight) run(ctx context.Context, checks *UpgradeChecks, spec kwerftv1.UpgradeSpec) ([]kwerftv1.UpgradeCheck, error) {
	switch spec.Component {
	case kwerftv1.UpgradeKwerft:
		return checks.Kwerft(ctx, spec, ""), nil
	case kwerftv1.UpgradeKubernetes:
		return checks.Kubernetes(ctx, spec, ""), nil
	}
	return nil, fmt.Errorf("unknown component %q", spec.Component)
}

// KubernetesTarget is the k3s version offered to an agent cluster running
// Kwerft release kwerft and Kubernetes kubernetes: that release's pin
// (upgrades.KubernetesTarget). It reads the release's manifest from the
// install repository (cached); never call it with the policy Off.
func (p *ConsolePreflight) KubernetesTarget(ctx context.Context, kwerft, kubernetes string) *kwerftv1.AvailableUpdate {
	kwerft = strings.TrimPrefix(kwerft, "v")
	if p.Local == nil || p.Local.Releases == nil || !upgrades.IsRelease(kwerft) || kubernetes == "" {
		return nil
	}
	p.mu.Lock()
	a, ok := p.manifests[kwerft]
	p.mu.Unlock()
	ttl := manifestTTL
	if ok && !a.ok {
		ttl = manifestErrTTL
	}
	if !ok || p.now().Sub(a.at) >= ttl {
		m, err := p.Local.Releases.Manifest(ctx, kwerft)
		a = cachedAnswer[*upgrades.Manifest]{v: m, at: p.now(), ok: err == nil}
		if err != nil {
			a.v = nil
		}
		p.mu.Lock()
		if p.manifests == nil {
			p.manifests = map[string]cachedAnswer[*upgrades.Manifest]{}
		}
		p.manifests[kwerft] = a
		p.mu.Unlock()
	}
	if a.v == nil {
		return nil
	}
	return upgrades.KubernetesTarget(a.v, kubernetes)
}
