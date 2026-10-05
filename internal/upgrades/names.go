package upgrades

import (
	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// LabelUpgrade marks the runner Job, its pod and the log ConfigMap of
	// one Upgrade.
	LabelUpgrade = "kwerft.dev/upgrade"

	// RunnerServiceAccount is the runner's identity (charts/kwerft
	// templates/upgrade-runner.yaml).
	RunnerServiceAccount = "kwerft-upgrade-runner"

	// HostWorkDir holds one directory per Upgrade on the installer node:
	// install.sh, SHA256SUMS, manifest.json, progress.jsonl.
	HostWorkDir = "/var/lib/kwerft/upgrade"
	// HostInstallLog is the installer's log.
	HostInstallLog = "/var/log/kwerft/install.log"
	// HostInstallEnv holds the installer's remembered flags (U2); a re-run
	// with --version and --yes needs nothing else.
	HostInstallEnv = "/var/lib/kwerft/install.env"
	// HostKubeconfig is what the host's helm uses, as the installer does.
	HostKubeconfig = "/etc/rancher/k3s/k3s.yaml"
	HostHelm       = "/usr/local/bin/helm"
	HostK3s        = "/usr/local/bin/k3s"

	// LogKey is the installer log tail's key in the log ConfigMap.
	LogKey = "install.log"
	// LogTailBytes is how much of the installer log is kept.
	LogTailBytes = 64 << 10

	// MinFreeBytes on the installer node and in the console's data volume.
	MinFreeBytes = 5 << 30

	// AnnotationFault injects a failure into a runner (e2e only; the
	// controller passes it on only with --upgrade-faults, chart value
	// e2e.faults): FaultInstall pretends the installer failed with exit 50
	// after it finished, FaultVerify fails verification.
	AnnotationFault = "kwerft.dev/e2e-fault"
	FaultInstall    = "install"
	FaultVerify     = "verify"

	// AnnotationRestoreDatabase on an Upgrade asks the console of the
	// version in its value to replace its database with
	// status.backup.database before it opens it (rollback of a release
	// with rollbackSafe: false); the console sets it to RestoreDone.
	AnnotationRestoreDatabase = "kwerft.dev/restore-database"
	RestoreDone               = "done"

	// BackupsDir is below the console's data directory; the newest
	// KeepDatabaseBackups copies are kept.
	BackupsDir          = "backups"
	KeepDatabaseBackups = 3
)

// UnitName is the installer's transient systemd unit.
func UnitName(upgrade string) string { return "kwerft-upgrade-" + upgrade }

// LogConfigMapName holds the installer log tail of an Upgrade.
func LogConfigMapName(upgrade string) string { return upgrade + "-log" }

// SnapshotName is the etcd snapshot taken before an Upgrade (k3s appends
// the node name and a timestamp).
func SnapshotName(upgrade string) string { return "pre-" + upgrade }

// Active phases hold the cluster: one at a time.
func Active(p kwerftv1.UpgradePhase) bool {
	switch p {
	case kwerftv1.UpgradePreflight, kwerftv1.UpgradeBackingUp, kwerftv1.UpgradeRunning,
		kwerftv1.UpgradeVerifying, kwerftv1.UpgradeRollingBack:
		return true
	}
	return false
}

// Finished phases never change again.
func Finished(p kwerftv1.UpgradePhase) bool {
	switch p {
	case kwerftv1.UpgradeSucceeded, kwerftv1.UpgradeRolledBack, kwerftv1.UpgradeFailed, kwerftv1.UpgradeCancelled:
		return true
	}
	return false
}

// RunnerStarted: the runner owns the Upgrade from Running on (cancel is no
// longer possible).
func RunnerStarted(p kwerftv1.UpgradePhase) bool {
	switch p {
	case kwerftv1.UpgradeRunning, kwerftv1.UpgradeVerifying, kwerftv1.UpgradeRollingBack:
		return true
	}
	return false
}
