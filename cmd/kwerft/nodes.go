package main

import (
	"cmp"
	"strings"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ehilzinger/kwerft/install"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/jointoken"
	"github.com/ehilzinger/kwerft/internal/version"
)

// setupNodes adds the node pool reconciler (Cloud servers as nodes of any
// cluster, management cluster only: it holds the NodePools and the Cloud
// token) and the node removal reconciler (drain and remove nodes on
// request, in every cluster). docs/phase5.md, W2.
func setupNodes(mgr ctrl.Manager, namespace string, dataKey []byte, registry clusters.Registry, consoleURL func() string) error {
	if err := (&controllers.NodeRemovalReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		return err
	}
	return (&controllers.NodePoolReconciler{
		Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Namespace: namespace,
		Clusters:     &controllers.RegistryClients{Registry: registry, Scheme: mgr.GetScheme()},
		JoinTokens:   jointoken.NewSigner(dataKey),
		ConsoleURL:   consoleURL,
		InstallerURL: install.ReleaseURL(version.Version),
		Version:      releaseVersion(version.Version),
	}).SetupWithManager(mgr)
}

// releaseVersion is the version for install.sh --version: releases only.
func releaseVersion(v string) string {
	if install.ReleaseURL(v) == "" {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// consoleURLFunc is https://<console host>, following Settings.
func consoleURLFunc(flagDomain string, active func() string) func() string {
	return func() string {
		host := flagDomain
		if active != nil {
			host = cmp.Or(active(), host)
		}
		if host == "" {
			return ""
		}
		return "https://" + host
	}
}
