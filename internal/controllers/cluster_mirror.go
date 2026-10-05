package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	corev1ac "k8s.io/client-go/applyconfigurations/core/v1"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Mirroring (docs/phase5.md, W3). NotificationChannels and GitConnections
// are kept in the management cluster only (the console writes them there),
// but every cluster needs them: its Alertmanager needs the receivers and
// its build pods the clone credentials. So the Cluster reconciler copies
// each one, with its Secret (notify-<name> in kwerft-observability,
// git-<name> in kwerft-builds), into every connected remote cluster, through
// the tunnel with Kwerft's own identity there, and removes copies whose
// original is gone. The copies are labelled LabelMirrored; the reconcilers
// there treat them as their own (render Alertmanager configs, check the
// credentials). They never set up Git webhooks: a remote cluster has no
// console hostname, so there is no webhook URL (deliveries reach the
// management console, which creates the Builds where they belong).
//
// A remote object of the same name that is not a copy is left alone and
// reported in the Mirrored condition.

const (
	// LabelMirrored marks the console's copies in a remote cluster.
	LabelMirrored = "kwerft.dev/mirrored"
	// ConditionMirrored on a Cluster reports the last mirroring pass.
	ConditionMirrored = "Mirrored"

	mirrorFieldOwner = "kwerft-mirror"
)

// mirror copies channels and Git connections into the remote cluster.
func (r *ClusterReconciler) mirror(ctx context.Context, remote client.Client) error {
	var problems []string
	var chans kwerftv1.NotificationChannelList
	if err := r.List(ctx, &chans); err != nil {
		return err
	}
	keep := map[string]bool{}
	for i := range chans.Items {
		ch := &chans.Items[i]
		if ch.DeletionTimestamp != nil {
			continue
		}
		keep[ch.Name] = true
		copyObj := &kwerftv1.NotificationChannel{
			TypeMeta:   metav1.TypeMeta{APIVersion: kwerftv1.GroupVersion.String(), Kind: "NotificationChannel"},
			ObjectMeta: metav1.ObjectMeta{Name: ch.Name},
			Spec:       ch.Spec,
		}
		if err := r.mirrorOne(ctx, remote, copyObj, &kwerftv1.NotificationChannel{},
			observability.Namespace, observability.ChannelSecret(ch.Name), LabelNotificationChannel); err != nil {
			problems = append(problems, fmt.Sprintf("notification channel %s: %v", ch.Name, err))
		}
	}
	if err := prune(ctx, remote, &kwerftv1.NotificationChannelList{}, keep, observability.Namespace, observability.ChannelSecret); err != nil {
		problems = append(problems, "notification channels: "+err.Error())
	}

	var conns kwerftv1.GitConnectionList
	if err := r.List(ctx, &conns); err != nil {
		return err
	}
	keep = map[string]bool{}
	for i := range conns.Items {
		gc := &conns.Items[i]
		if gc.DeletionTimestamp != nil {
			continue
		}
		keep[gc.Name] = true
		copyObj := &kwerftv1.GitConnection{
			TypeMeta:   metav1.TypeMeta{APIVersion: kwerftv1.GroupVersion.String(), Kind: "GitConnection"},
			ObjectMeta: metav1.ObjectMeta{Name: gc.Name},
			Spec:       gc.Spec,
		}
		if err := r.mirrorOne(ctx, remote, copyObj, &kwerftv1.GitConnection{},
			builds.Namespace, builds.CredentialsSecret(gc.Name), LabelGitConnection); err != nil {
			problems = append(problems, fmt.Sprintf("Git connection %s: %v", gc.Name, err))
		}
	}
	if err := prune(ctx, remote, &kwerftv1.GitConnectionList{}, keep, builds.Namespace, builds.CredentialsSecret); err != nil {
		problems = append(problems, "Git connections: "+err.Error())
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// errNotACopy: the remote cluster has an object of that name of its own.
var errNotACopy = errors.New("the cluster has one of its own by that name, which Kwerft leaves alone")

// mirrorOne applies copyObj in the remote cluster, then its Secret
// (namespace/secretName, labelled ownerLabel=<name>) owned by the copy, as
// the remote reconciler expects (it never adopts another owner's Secret).
func (r *ClusterReconciler) mirrorOne(ctx context.Context, remote client.Client, copyObj, current client.Object, namespace, secretName, ownerLabel string) error {
	name := copyObj.GetName()
	switch err := remote.Get(ctx, client.ObjectKey{Name: name}, current); {
	case err == nil && current.GetLabels()[LabelMirrored] != "true":
		return errNotACopy
	case err != nil && !apierrors.IsNotFound(err):
		return err
	}
	copyObj.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelMirrored: "true"})
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(copyObj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: raw}
	unstructured.RemoveNestedField(u.Object, "status")
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	if err := remote.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(mirrorFieldOwner), client.ForceOwnership); err != nil {
		return err
	}
	if err := remote.Get(ctx, client.ObjectKey{Name: name}, current); err != nil {
		return err
	}

	var local corev1.Secret
	switch err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, &local); {
	case apierrors.IsNotFound(err):
		return nil // nothing stored yet; the next pass copies it
	case err != nil:
		return err
	}
	gvk := copyObj.GetObjectKind().GroupVersionKind()
	s := corev1ac.Secret(secretName, namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelMirrored: "true", ownerLabel: name}).
		WithOwnerReferences(metav1ac.OwnerReference().
			WithAPIVersion(gvk.GroupVersion().String()).WithKind(gvk.Kind).
			WithName(name).WithUID(current.GetUID()).
			WithController(true).WithBlockOwnerDeletion(true)).
		WithType(corev1.SecretTypeOpaque).
		WithData(local.Data)
	if err := remote.Apply(ctx, s, client.FieldOwner(mirrorFieldOwner), client.ForceOwnership); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("namespace %s is missing there (the installer creates it)", namespace)
		}
		return err
	}
	return nil
}

// prune deletes the remote copies whose original is gone, and their Secrets
// (namespace/secretName(name)); garbage collection would remove them too,
// a little later, since the copy owns them.
func prune(ctx context.Context, remote client.Client, list client.ObjectList, keep map[string]bool, namespace string, secretName func(string) string) error {
	if err := remote.List(ctx, list, client.MatchingLabels{LabelMirrored: "true"}); err != nil {
		return err
	}
	items, err := meta.ExtractList(list)
	if err != nil {
		return err
	}
	for _, item := range items {
		obj, ok := item.(client.Object)
		if !ok || keep[obj.GetName()] {
			continue
		}
		if err := client.IgnoreNotFound(remote.Delete(ctx, obj)); err != nil {
			return err
		}
		// Only a Secret that is a copy too.
		var s corev1.Secret
		switch err := remote.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName(obj.GetName())}, &s); {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case s.Labels[LabelMirrored] == "true":
			if err := client.IgnoreNotFound(remote.Delete(ctx, &s, client.Preconditions{UID: &s.UID})); err != nil {
				return err
			}
		}
	}
	return nil
}

// setMirrored records the outcome of a mirroring pass on the Cluster.
func setMirrored(c *kwerftv1.Cluster, err error) {
	cond := metav1.Condition{Type: ConditionMirrored, Status: metav1.ConditionTrue, Reason: "Synced", ObservedGeneration: c.Generation,
		Message: "Notification channels and Git connections are copied into this cluster."}
	if err != nil {
		cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "Failed", "Copying notification channels and Git connections: "+err.Error()
	}
	meta.SetStatusCondition(&c.Status.Conditions, cond)
}
