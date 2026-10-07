// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// The hcloud cloud-controller-manager and CSI driver read their token from
// the Secret kube-system/hcloud (key "token"), which install.sh creates
// when it installs them, labelled as Kwerft's. When the token changes in
// Settings, the reconciler copies it there and restarts both (they read it
// only when they start). A Secret hcloud that Kwerft did not create is
// never touched, and a removed token never blanks theirs.

const (
	// HCloudSystemSecret is the CCM's and CSI driver's token Secret.
	HCloudSystemSecret    = "hcloud"
	HCloudSystemNamespace = "kube-system"
	// annotationHCloudTokenSum on their pod templates restarts them.
	annotationHCloudTokenSum = "kwerft.dev/hcloud-token-sum"
)

// hcloudDeployments read the token; install.sh's release names.
var hcloudDeployments = []string{"hcloud-cloud-controller-manager", "hcloud-csi-controller"}

// systemHCloudToken reads the CCM's and CSI driver's token ("" when none).
func systemHCloudToken(ctx context.Context, c client.Reader) (string, error) {
	var sec corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &sec)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(sec.Data[HCloudTokenKey])), nil
}

func (r *HetznerCloudReconciler) mirrorToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	var sec corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &sec)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if sec.Labels[LabelManagedBy] != ManagedByKwerft || string(sec.Data[HCloudTokenKey]) == token {
		return nil
	}
	patch, _ := json.Marshal(map[string]any{"data": map[string][]byte{HCloudTokenKey: []byte(token)}})
	if err := r.Patch(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}},
		client.RawPatch(types.MergePatchType, patch)); err != nil {
		return err
	}
	if err := restartHCloudDeployments(ctx, r.Client, token); err != nil {
		return err
	}
	log.FromContext(ctx).Info("gave the hcloud CCM and CSI driver the new Cloud API token")
	return nil
}

// tokenSum is a short digest of a Cloud API token: restart annotations and
// change markers carry it, never the token.
func tokenSum(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:6])
}

// restartHCloudDeployments restarts the CCM and CSI controller (they read
// the token only when they start); missing ones are skipped.
func restartHCloudDeployments(ctx context.Context, c client.Client, token string) error {
	restart, _ := json.Marshal(map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{annotationHCloudTokenSum: tokenSum(token)}}}}})
	for _, name := range hcloudDeployments {
		d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: name}}
		if err := c.Patch(ctx, d, client.RawPatch(types.StrategicMergePatchType, restart)); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
