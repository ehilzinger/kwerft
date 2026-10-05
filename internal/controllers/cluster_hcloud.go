package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Hetzner Cloud in remote clusters (docs/phase5.md › As built (W1), remote
// clusters). A hetzner-cloud cluster's servers, volumes and Load Balancer
// live in the console's Cloud project, so the Cluster reconciler hands the
// cluster that project's token — only as the CCM's and CSI driver's Secret
// kube-system/hcloud, which the cluster's own Hetzner Cloud reconciler
// falls back to — and its Cloud Firewall and Load Balancer settings
// (Cluster.spec.hetznerCloud) as spec.hetznerCloud of its ConsoleSettings,
// where that reconciler reads them. What it reports there comes back into
// the Cluster's status.hetznerCloud. All through the tunnel, as Kwerft's
// own identity in that cluster.

const (
	// ConditionHetznerCloud on a Cluster reports the last hand-over.
	ConditionHetznerCloud = "HetznerCloud"
	// AnnotationHCloudTokenSum on a remote cluster's ConsoleSettings changes
	// with the token it was handed, so its reconciler syncs again at once.
	AnnotationHCloudTokenSum = "kwerft.dev/hcloud-token-sum"
)

// syncCloud hands a connected hetzner-cloud cluster the token and its
// settings, and copies its report into c.Status. The note explains a
// hand-over that ran but is incomplete (no token stored, or the cluster's
// own kube-system/hcloud).
func (r *ClusterReconciler) syncCloud(ctx context.Context, remote client.Client, c *kwerftv1.Cluster) (string, error) {
	token, err := HCloudToken(ctx, r.APIReader)
	if err != nil {
		return "", err
	}
	note, sum := "", ""
	switch {
	case token == "":
		note = "No Hetzner Cloud API token is stored in Settings: the cluster's Cloud Firewall, Load Balancer and Cloud Volumes need it."
	default:
		own, err := handCloudToken(ctx, remote, token)
		if err != nil {
			return "", fmt.Errorf("the Cloud API token: %w", err)
		}
		if own {
			note = "The cluster's Secret kube-system/hcloud is its own, so Kwerft leaves its token alone."
		} else {
			sum = tokenSum(token)
		}
	}

	spec, err := runtime.DefaultUnstructuredConverter.ToUnstructured(c.Spec.HetznerCloud.CloudSettings())
	if err != nil {
		return "", err
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kwerftv1.GroupVersion.String(), "kind": "ConsoleSettings",
		"metadata": map[string]any{"name": kwerftv1.ConsoleSettingsName},
		"spec":     map[string]any{"hetznerCloud": spec},
	}}
	if sum != "" {
		u.SetAnnotations(map[string]string{AnnotationHCloudTokenSum: sum})
	}
	if err := remote.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(mirrorFieldOwner), client.ForceOwnership); err != nil {
		return "", fmt.Errorf("the Cloud settings: %w", err)
	}
	var s kwerftv1.ConsoleSettings
	if err := remote.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		return "", err
	}
	c.Status.HetznerCloud = s.Status.HetznerCloud.DeepCopy()
	return note, nil
}

// handCloudToken keeps the remote kube-system/hcloud holding token, and
// restarts the CCM and CSI controller there when it changed. An operator's
// own Secret of that name (not labelled as Kwerft's) is left alone: own.
func handCloudToken(ctx context.Context, remote client.Client, token string) (own bool, err error) {
	var sec corev1.Secret
	err = remote.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &sec)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return false, err
	case sec.Labels[LabelManagedBy] != ManagedByKwerft:
		return true, nil
	case string(sec.Data[HCloudTokenKey]) == token:
		return false, nil
	}
	existed := err == nil
	// Only the token key: the installer's network key (for the CCM) stays.
	s := corev1ac.Secret(HCloudSystemSecret, HCloudSystemNamespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithType(corev1.SecretTypeOpaque).
		WithData(map[string][]byte{HCloudTokenKey: []byte(token)})
	if err := remote.Apply(ctx, s, client.FieldOwner(mirrorFieldOwner), client.ForceOwnership); err != nil {
		return false, err
	}
	if existed {
		return false, restartHCloudDeployments(ctx, remote, token)
	}
	return false, nil
}

// setCloudSynced records the outcome of a hand-over on the Cluster.
func setCloudSynced(c *kwerftv1.Cluster, note string, err error) {
	cond := metav1.Condition{Type: ConditionHetznerCloud, Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: c.Generation,
		Message: "The cluster has the Cloud API token and follows its Cloud Firewall and Load Balancer settings."}
	switch {
	case err != nil:
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Failed", "Handing over the Hetzner Cloud settings: "+err.Error()
	case note != "":
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Incomplete", note
	}
	meta.SetStatusCondition(&c.Status.Conditions, cond)
}
