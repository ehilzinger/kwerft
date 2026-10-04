package main

import (
	"log/slog"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/kube"
)

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
