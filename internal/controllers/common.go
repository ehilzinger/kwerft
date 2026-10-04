// Package controllers holds the reconcilers that turn kwerft.dev resources into
// native Kubernetes objects. All writes use server-side apply with the "kwerft"
// field manager, so hand edits to fields Kwerft does not manage survive.
package controllers

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	// FieldOwner is the server-side apply field manager for everything Kwerft writes.
	FieldOwner = "kwerft"

	LabelManagedBy  = "app.kubernetes.io/managed-by"
	ManagedByKwerft = "kwerft"
	// LabelProject marks a namespace as a Kwerft project and pods as belonging to one.
	LabelProject = "kwerft.dev/project"
	// LabelApp selects the pods of one App.
	LabelApp = "kwerft.dev/app"
	// LabelSystem marks namespaces whose pods may reach every app (ingress,
	// monitoring). Set on platform namespaces by the installer.
	LabelSystem = "kwerft.dev/system"

	// The shared Gateway that public traffic enters through (see the Helm chart).
	GatewayName      = "kwerft"
	GatewayNamespace = "kwerft-system"

	// ConditionReady is the single summary condition on every Kwerft resource.
	ConditionReady = "Ready"
)

// NewScheme returns a scheme with every type the controllers read or write.
func NewScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	must(clientgoscheme.AddToScheme(s))
	must(kwerftv1.AddToScheme(s))
	must(gwv1.Install(s))
	return s
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func apply(ctx context.Context, c client.Client, obj runtime.ApplyConfiguration) error {
	return c.Apply(ctx, obj, client.FieldOwner(FieldOwner), client.ForceOwnership)
}

// controllerRef makes owner the controlling owner, so deleting owner
// garbage-collects the object.
func controllerRef(owner metav1.Object, gvk schema.GroupVersionKind) *metav1ac.OwnerReferenceApplyConfiguration {
	return metav1ac.OwnerReference().
		WithAPIVersion(gvk.GroupVersion().String()).
		WithKind(gvk.Kind).
		WithName(owner.GetName()).
		WithUID(owner.GetUID()).
		WithController(true).
		WithBlockOwnerDeletion(true)
}

// deleteIfControlledBy removes obj (identified by name/namespace) only when
// owner controls it, so Kwerft never deletes objects it did not create.
func deleteIfControlledBy(ctx context.Context, c client.Client, obj client.Object, owner metav1.Object) error {
	if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(obj, owner) {
		return nil
	}
	return client.IgnoreNotFound(c.Delete(ctx, obj))
}

func setReady(conds *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}
