// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"fmt"
	"path"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The runner Job (docs/phase6-upgrades.md › Architecture): `kwerft
// upgrade-runner` from the running console's image, pinned by digest, on
// the node the installer ran on, with the host's network and file system.
// Only the Upgrade controller creates it; its identity is the chart's
// kwerft-upgrade-runner service account.

const (
	// RunnerDeadline bounds a runner Job: installer (InstallTimeout),
	// verification and a rollback.
	RunnerDeadline = 3 * 60 * 60
	// runnerBackoff: runner pods that fail (an API hiccup) are replaced and
	// resume from the Upgrade's status.
	runnerBackoff = 4
	runnerTTL     = 7 * 24 * 60 * 60

	runnerHostRoot = "/host"
	runnerWorkDir  = "/work"
)

// RunnerJobName is the runner Job of an Upgrade.
func RunnerJobName(upgrade string) string {
	name := upgrade + "-runner"
	if len(name) > 63 {
		name = name[:63]
	}
	for len(name) > 0 && name[len(name)-1] == '-' {
		name = name[:len(name)-1]
	}
	return name
}

func (r *UpgradeReconciler) runnerJob(u *kwerftv1.Upgrade, image string) *batchv1.Job {
	labels := map[string]string{upgrades.LabelUpgrade: u.Name, LabelManagedBy: ManagedByKwerft}
	args := []string{"upgrade-runner",
		"--upgrade=" + u.Name,
		"--host-root=" + runnerHostRoot,
		"--work-dir=" + path.Join(runnerWorkDir, u.Name),
		"--host-work-dir=" + path.Join(upgrades.HostWorkDir, u.Name),
	}
	if r.InstallBaseURL != "" {
		args = append(args, "--install-base-url="+r.InstallBaseURL)
	}
	if f := u.Annotations[upgrades.AnnotationFault]; f != "" && r.Faults {
		args = append(args, "--fault="+f)
	}
	hostPathDir := corev1.HostPathDirectory
	hostPathCreate := corev1.HostPathDirectoryOrCreate
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: RunnerJobName(u.Name), Namespace: r.ns(), Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kwerftv1.GroupVersion.String(), Kind: "Upgrade", Name: u.Name, UID: u.UID,
				Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](runnerBackoff),
			ActiveDeadlineSeconds:   ptr.To[int64](RunnerDeadline),
			TTLSecondsAfterFinished: ptr.To[int32](runnerTTL),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: upgrades.RunnerServiceAccount,
					RestartPolicy:      corev1.RestartPolicyNever,
					// The host's network: a Cilium upgrade does not cut the
					// runner off.
					HostNetwork:  true,
					DNSPolicy:    corev1.DNSClusterFirstWithHostNet,
					NodeSelector: map[string]string{kwerftv1.LabelInstaller: "true"},
					Tolerations:  []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
					Containers: []corev1.Container{{
						Name:  "runner",
						Image: image,
						Args:  args,
						Env: []corev1.EnvVar{
							{Name: "NODE_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
							{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
						},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("32Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
						// Root on the host through systemd-run (chrooted into
						// the host's /, read-only): CAP_SYS_CHROOT only.
						// AppArmor Unconfined: containerd's default profile
						// keeps a pod off the system bus, so systemd-run gets
						// "Access denied"; the runner is root on the host
						// through systemd anyway (upgrades.SystemdHost).
						SecurityContext: &corev1.SecurityContext{
							AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
							RunAsUser:                ptr.To[int64](0),
							RunAsNonRoot:             ptr.To(false),
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_CHROOT"}},
							SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "host-root", MountPath: runnerHostRoot, ReadOnly: true, MountPropagation: ptr.To(corev1.MountPropagationHostToContainer)},
							{Name: "work", MountPath: runnerWorkDir},
							{Name: "tmp", MountPath: "/tmp"},
						},
					}},
					Volumes: []corev1.Volume{
						{Name: "host-root", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: &hostPathDir}}},
						{Name: "work", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: upgrades.HostWorkDir, Type: &hostPathCreate}}},
						{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("16Mi"))}}},
					},
				},
			},
		},
	}
}

// jobFailure describes a runner Job that gave up, or "".
func jobFailure(job *batchv1.Job) (reason, msg string) {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				return "Timeout", fmt.Sprintf("The upgrade runner ran longer than %d hours and was stopped.", RunnerDeadline/3600)
			}
			return "Runner", "The upgrade runner failed: " + c.Message
		}
	}
	return "", ""
}
