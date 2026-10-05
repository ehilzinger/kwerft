package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/server"
	"github.com/ehilzinger/kwerft/internal/upgrades"
	"github.com/ehilzinger/kwerft/internal/version"
)

// upgradeOptions configure upgrades (docs/phase6-upgrades.md).
type upgradeOptions struct {
	namespace      string
	installBaseURL string
	faults         bool
	// console: the management cluster, which runs release discovery and
	// the update policy and owns the database.
	console  bool
	cluster  string
	dataDir  string
	database upgrades.Snapshotter
}

// newUpgradeChecks is the Kwerft upgrade preflight of this cluster. The
// Upgrade controller and the console API (Settings › Updates, before it
// creates an Upgrade) run the same checks.
func newUpgradeChecks(mgr ctrl.Manager, opt upgradeOptions, source upgrades.Source, self func(context.Context) (controllers.SelfImage, error)) *controllers.UpgradeChecks {
	checks := &controllers.UpgradeChecks{
		Reader:   mgr.GetClient(),
		Releases: source,
		Registry: &upgrades.OCIRegistry{},
		Version:  strings.TrimPrefix(version.Version, "v"),
		Self:     self,
		Cluster:  opt.cluster,
	}
	if opt.console && opt.database != nil {
		checks.DataDir = opt.dataDir
		checks.FreeBytes = (&upgrades.SystemdHost{Root: "/"}).FreeBytes
	}
	return checks
}

// setupUpgrades adds the Upgrade reconciler (every cluster) and release
// discovery (the console's only), and returns the preflight for the
// console API. The database copy runs in this process, which owns the
// store: the controller and the console are one binary and one pod
// (replicas: 1, leader election hands over on shutdown).
func setupUpgrades(mgr ctrl.Manager, opt upgradeOptions) (*controllers.UpgradeChecks, error) {
	source := &upgrades.HTTPSource{BaseURL: opt.installBaseURL}
	self := selfImage(mgr.GetAPIReader(), opt.namespace)
	checks := newUpgradeChecks(mgr, opt, source, self)
	r := &controllers.UpgradeReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Namespace: opt.namespace,
		Checks:         checks,
		RunnerImage:    runnerImage(self),
		InstallBaseURL: opt.installBaseURL,
		Faults:         opt.faults,
	}
	if opt.console && opt.database != nil {
		r.Database, r.DataDir = opt.database, opt.dataDir
	}
	if err := r.SetupWithManager(mgr); err != nil {
		return nil, err
	}
	if !opt.console {
		return checks, nil
	}
	return checks, (&controllers.UpdatesReconciler{Client: mgr.GetClient(), Source: source,
		Version: strings.TrimPrefix(version.Version, "v")}).SetupWithManager(mgr)
}

// upgradePreflight is the console API's preflight (server.UpgradePreflight):
// the Upgrade controller's own checks, run before the API creates an
// Upgrade.
type upgradePreflight struct {
	local *controllers.UpgradeChecks
	// kubernetes is the k3s preflight; nil: Kubernetes upgrades are not
	// available yet, and the preflight says so.
	// TODO(U4): wire the Kubernetes upgrade preflight here.
	kubernetes func(ctx context.Context, spec kwerftv1.UpgradeSpec) []kwerftv1.UpgradeCheck
}

// Supports: the local cluster only so far.
// TODO(U4): remote clusters (an Upgrade created there through the tunnel,
// checked with that cluster's reader and the agent's version).
func (p *upgradePreflight) Supports(cluster string) bool { return cluster == clusters.Local }

func (p *upgradePreflight) Preflight(ctx context.Context, cluster string, spec kwerftv1.UpgradeSpec) ([]kwerftv1.UpgradeCheck, error) {
	if !p.Supports(cluster) {
		return nil, server.ErrUpgradeUnsupported
	}
	switch spec.Component {
	case kwerftv1.UpgradeKwerft:
		return p.local.Kwerft(ctx, spec, ""), nil
	case kwerftv1.UpgradeKubernetes:
		if p.kubernetes != nil {
			return p.kubernetes(ctx, spec), nil
		}
		return []kwerftv1.UpgradeCheck{{Check: controllers.CheckTarget, Message: "Kubernetes upgrades from the console are not available in this release."}}, nil
	}
	return nil, fmt.Errorf("unknown component %q", spec.Component)
}

// selfImage reads the console's own pod (POD_NAME, set by the chart): the
// image its Deployment names and the digest the node pulled.
func selfImage(reader client.Reader, namespace string) func(context.Context) (controllers.SelfImage, error) {
	return func(ctx context.Context) (controllers.SelfImage, error) {
		name := os.Getenv("POD_NAME")
		if name == "" {
			return controllers.SelfImage{}, fmt.Errorf("POD_NAME is not set")
		}
		var pod corev1.Pod
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			return controllers.SelfImage{}, err
		}
		var out controllers.SelfImage
		for _, c := range pod.Spec.Containers {
			if c.Name == "kwerft" {
				out.Image = c.Image
			}
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "kwerft" {
				out.ImageID = cs.ImageID
			}
		}
		if out.Image == "" {
			return out, fmt.Errorf("pod %s has no kwerft container", name)
		}
		return out, nil
	}
}

// runnerImage is the running console image pinned by digest.
func runnerImage(self func(context.Context) (controllers.SelfImage, error)) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		s, err := self(ctx)
		if err != nil {
			return "", err
		}
		return upgrades.DigestRef(s.Image, s.ImageID)
	}
}

// restoreDatabase puts the pre-upgrade copy back when a rollback asked
// this version for it (upgrades.RestorePendingDatabase), before the store
// is opened. Problems are logged; the console starts either way.
func restoreDatabase(ctx context.Context, log *slog.Logger, dataDir, dbFile string) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return // no cluster (development)
	}
	c, err := client.New(cfg, client.Options{Scheme: controllers.NewScheme()})
	if err != nil {
		log.Warn("cannot check for a database restore", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := upgrades.RestorePendingDatabase(ctx, c, dataDir, dbFile, version.Version, log); err != nil {
		log.Error("database restore after an upgrade rollback failed", "err", err)
	}
}
