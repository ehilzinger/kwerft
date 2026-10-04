package main

import (
	"context"
	"log/slog"

	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/kube"
)

// setupClusters adds the Cluster reconciler, which runs in the management
// cluster only: the local Cluster, hetzner-cloud creation, and each remote
// cluster's status from its agent on hub.
func setupClusters(mgr ctrl.Manager, hub *clusters.Hub, namespace, consoleDomain string) error {
	cs, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return err
	}
	return (&controllers.ClusterReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Tunnel: hub,
		Namespace: namespace, ConsoleDomain: consoleDomain,
		Local: func(ctx context.Context) clusters.AgentInfo { return clusters.LocalInfo(ctx, cs, "") },
	}).SetupWithManager(mgr)
}

// workloadAccess returns what the workload API needs to act as signed-in
// users. With a manager it shares the manager's HTTP transport, RESTMapper
// and informer cache; without one (--controllers=false) it connects on its
// own if a kubeconfig is available, and otherwise leaves the workload API
// disabled.
func workloadAccess(log *slog.Logger, mgr ctrl.Manager) (*kube.Impersonator, client.Reader) {
	if mgr != nil {
		imp, err := kube.NewImpersonator(mgr.GetConfig(), mgr.GetHTTPClient(), mgr.GetRESTMapper(), mgr.GetScheme())
		if err != nil {
			log.Error("workload API disabled", "err", err)
			return nil, nil
		}
		return imp, mgr.GetCache()
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Warn("no cluster configured: workload API disabled", "err", err)
		return nil, nil
	}
	imp, err := kube.NewImpersonator(cfg, nil, nil, controllers.NewScheme())
	if err != nil {
		log.Error("workload API disabled", "err", err)
		return nil, nil
	}
	return imp, nil
}
