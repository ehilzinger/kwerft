// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/upgrades"
	"github.com/ehilzinger/kwerft/internal/version"
)

// runUpgradeRunner is `kwerft upgrade-runner`: the pod the Upgrade
// controller starts on the installer node for a Kwerft upgrade
// (internal/controllers/upgrade_runner.go, internal/upgrades/runner.go).
// It runs the console image that was running when the upgrade started, so
// it can roll a broken new release back.
func runUpgradeRunner(args []string) int {
	fs := flag.NewFlagSet("upgrade-runner", flag.ExitOnError)
	name := fs.String("upgrade", "", "the Upgrade to run")
	namespace := fs.String("namespace", envOr("POD_NAMESPACE", "kwerft-system"), "namespace of the console and the log ConfigMap")
	hostRoot := fs.String("host-root", "/host", "where the host's / is mounted (read-only); systemd-run runs chrooted there")
	workDir := fs.String("work-dir", "", "this Upgrade's directory as the runner sees it")
	hostWorkDir := fs.String("host-work-dir", "", "the same directory on the host")
	baseURL := fs.String("install-base-url", upgrades.DefaultInstallBaseURL, "the install repository's raw files")
	fault := fs.String("fault", "", "inject a failure: install or verify (e2e only)")
	apiServer := fs.String("api-server", "https://127.0.0.1:6443", "the Kubernetes API on this server (not through the Service, which Cilium carries); empty: in-cluster")
	_ = fs.Parse(args)

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("upgrade", *name)
	if *name == "" || *workDir == "" || *hostWorkDir == "" {
		log.Error("--upgrade, --work-dir and --host-work-dir are required")
		return 2
	}
	cfg, err := ctrl.GetConfig()
	if err != nil {
		log.Error("no cluster configuration", "err", err)
		return 1
	}
	if *apiServer != "" {
		cfg.Host = *apiServer
	}
	scheme := controllers.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		log.Error("scheme", "err", err)
		return 1
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		log.Error("cannot create the Kubernetes client", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	r := &upgrades.Runner{
		Name:    *name,
		Cluster: &upgrades.KubeCluster{Client: c, Name: *name, Namespace: *namespace},
		Host:    &upgrades.SystemdHost{Root: *hostRoot},
		Files:   &upgrades.HTTPSource{BaseURL: *baseURL},
		Dir:     *workDir, HostDir: *hostWorkDir,
		Fault: *fault,
		Log:   log,
	}
	log.Info("kwerft upgrade runner starting", "version", version.Version, "node", os.Getenv("NODE_NAME"), "fault", *fault)
	// Every step resumes from the Upgrade's status: a transient error (the
	// API while the console restarts) is retried here for a while, then by
	// the Job's next pod.
	var failingSince, lastErr time.Time
	for {
		err := r.Run(ctx)
		if err == nil {
			return 0
		}
		if ctx.Err() != nil {
			log.Info("upgrade runner stopped; the next pod resumes", "err", err)
			return 1
		}
		if time.Since(lastErr) > time.Minute {
			failingSince = time.Now() // the last run got somewhere
		}
		lastErr = time.Now()
		log.Warn("upgrade runner: retrying", "err", err)
		if time.Since(failingSince) > 10*time.Minute {
			log.Error("upgrade runner gives up; the Job starts another", "err", err)
			return 1
		}
		select {
		case <-ctx.Done():
			return 1
		case <-time.After(10 * time.Second):
		}
	}
}
