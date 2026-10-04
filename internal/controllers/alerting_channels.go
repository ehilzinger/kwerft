package controllers

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	rbacv1ac "k8s.io/client-go/applyconfigurations/rbac/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Notification channels. The reconciler
//   - creates the channel's Secret notify-<name> in kwerft-observability,
//     empty and owned by the channel, and keeps the Role
//     kwerft:notify-secrets there listing exactly these Secrets with the
//     verb patch, bound to owners and admins: Git connections' write-only
//     pattern (gitconnection_controller.go). The console writes webhook URLs,
//     passwords and tokens as the signed-in user; nobody gets them back;
//   - renders the VMAlertmanagerConfig kwerft-<name> (internal/alerting,
//     receivers.go): one receiver whose credentials the operator reads from
//     the Secret, and a route for the enabled rules naming the channel;
//   - reports Ready: credentials missing, Alertmanager adding its namespace
//     matcher (see receivers.go), or the operator rejecting the config.
//
// Secrets are read uncached (APIReader), only to know which keys are set.

const (
	// NotifySecretsRole is the Role (and RoleBinding) in kwerft-observability
	// that lets owners and admins patch the channels' Secrets.
	NotifySecretsRole = "kwerft:notify-secrets"
	// LabelNotificationChannel marks a channel's Secret and config.
	LabelNotificationChannel = "kwerft.dev/notification-channel"
	// ChannelConfigPrefix + channel name is its VMAlertmanagerConfig.
	ChannelConfigPrefix = "kwerft-"

	channelRecheck = 10 * time.Minute
	// channelWaitForSecret: the console writes credentials right after
	// creating the channel.
	channelWaitForSecret = time.Minute
)

// NotificationChannelReconciler manages channels' Secrets, RBAC and
// Alertmanager configuration.
type NotificationChannelReconciler struct {
	client.Client
	// APIReader reads Secrets; nil falls back to the client (tests).
	APIReader client.Reader
	Now       func() time.Time
}

func (r *NotificationChannelReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *NotificationChannelReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *NotificationChannelReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var ch kwerftv1.NotificationChannel
	err := r.Get(ctx, req.NamespacedName, &ch)
	if apierrors.IsNotFound(err) {
		if err := r.deleteOrphans(ctx, req.Name); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.syncRole(ctx)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ch.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := r.syncRole(ctx); err != nil {
		return ctrl.Result{}, err
	}
	sec, err := r.ensureSecret(ctx, &ch)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.report(ctx, &ch, false, metav1.ConditionFalse, "NoMonitoringStack",
				"The namespace "+observability.Namespace+" does not exist; the installer creates it (stage Observability).", time.Minute)
		case isTerminal(err):
			return r.report(ctx, &ch, false, metav1.ConditionFalse, reasonOf(err), err.Error(), time.Minute)
		}
		return ctrl.Result{}, err
	}
	keys := map[string]bool{}
	for k, v := range sec.Data {
		if len(v) > 0 {
			keys[k] = true
		}
	}
	required, what := alerting.RequiredSecret(&ch.Spec)
	secretSet := required != "" && keys[required] || ch.Spec.Type == kwerftv1.NotifyNtfy && keys[observability.KeyToken]

	rules, err := r.rulesFor(ctx, ch.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	cfg := &unstructured.Unstructured{}
	cfg.SetGroupVersionKind(VMAlertmanagerConfigGVK)
	cfg.SetName(ChannelConfigPrefix + ch.Name)
	cfg.SetNamespace(observability.Namespace)
	cfg.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft, LabelNotificationChannel: ch.Name})
	cfg.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: kwerftv1.GroupVersion.String(), Kind: "NotificationChannel", Name: ch.Name, UID: ch.UID,
		Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true),
	}})
	cfg.Object["spec"] = alerting.AlertmanagerConfigSpec(alerting.ChannelConfig{
		Channel: &ch, SecretKeys: keys, SecretVersion: sec.ResourceVersion, Rules: rules,
	})
	err = r.Apply(ctx, client.ApplyConfigurationFromUnstructured(cfg), client.FieldOwner(FieldOwner), client.ForceOwnership)
	if meta.IsNoMatchError(err) {
		return r.report(ctx, &ch, secretSet, metav1.ConditionFalse, "NoMonitoringStack",
			"The VictoriaMetrics operator is not installed (install.sh stage Observability).", time.Minute)
	}
	if err != nil {
		return ctrl.Result{}, err
	}

	if required != "" && !keys[required] {
		since := ch.CreationTimestamp.Time
		if t, err := time.Parse(time.RFC3339Nano, ch.Annotations[AnnotationCredentialsUpdated]); err == nil && t.After(since) {
			since = t
		}
		if r.now().Sub(since) < channelWaitForSecret {
			return r.report(ctx, &ch, false, metav1.ConditionUnknown, "WaitingForCredentials", "Waiting for the "+what+".", 2*time.Second)
		}
		return r.report(ctx, &ch, false, metav1.ConditionFalse, "SecretMissing", "No "+what+" is stored. Edit the channel and enter it.", 0)
	}
	if reason, msg := r.alertmanagerProblem(ctx); reason != "" {
		return r.report(ctx, &ch, secretSet, metav1.ConditionFalse, reason, msg, time.Minute)
	}
	var got unstructured.Unstructured
	got.SetGroupVersionKind(VMAlertmanagerConfigGVK)
	if err := r.Get(ctx, client.ObjectKeyFromObject(cfg), &got); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	after := channelRecheck
	if observed, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration"); observed < got.GetGeneration() {
		after = vmPoll // the operator has not looked yet
	} else if msg := operatorRejection(&got); msg != "" {
		return r.report(ctx, &ch, secretSet, metav1.ConditionFalse, "RejectedByAlertmanager", "Alertmanager rejected the channel: "+msg, vmPoll)
	}
	msg := "No rule notifies this channel yet. Add it to rules under Alert rules."
	if len(rules) > 0 {
		msg = "Receives the alerts of " + plural(len(rules), "rule ", "rules ") + strings.Join(rules, ", ") + "."
	}
	return r.report(ctx, &ch, secretSet, metav1.ConditionTrue, "Routing", msg, after)
}

// report writes the status if it changed, then requeues after. The
// console's test results (lastTest, lastTestError) are left alone.
func (r *NotificationChannelReconciler) report(ctx context.Context, ch *kwerftv1.NotificationChannel, secretSet bool,
	status metav1.ConditionStatus, reason, msg string, after time.Duration) (ctrl.Result, error) {
	orig := ch.DeepCopy()
	ch.Status.ObservedGeneration = ch.Generation
	ch.Status.SecretSet = secretSet
	setReady(&ch.Status.Conditions, ch.Generation, status, reason, msg)
	if !equality.Semantic.DeepEqual(orig.Status, ch.Status) {
		if err := patchStatus(ctx, r.Client, ch, orig); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

// rulesFor lists the enabled rules that name the channel.
func (r *NotificationChannelReconciler) rulesFor(ctx context.Context, channel string) ([]string, error) {
	var list kwerftv1.AlertRuleList
	if err := r.List(ctx, &list); err != nil {
		return nil, err
	}
	var out []string
	for _, rule := range list.Items {
		if rule.DeletionTimestamp.IsZero() && !rule.Spec.Disabled && slices.Contains(rule.Spec.Channels, channel) {
			out = append(out, rule.Name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// alertmanagerProblem checks the VMAlertmanager the stack runs: without
// disableNamespaceMatcher the operator confines each VMAlertmanagerConfig's
// route to alerts from its own namespace, and Kwerft's alerts carry their
// project's namespace or none.
func (r *NotificationChannelReconciler) alertmanagerProblem(ctx context.Context) (reason, msg string) {
	var list unstructured.UnstructuredList
	list.SetGroupVersionKind(VMAlertmanagerGVK)
	if err := r.List(ctx, &list, client.InNamespace(observability.Namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return "NoMonitoringStack", "The VictoriaMetrics operator is not installed (install.sh stage Observability)."
		}
		return "AlertmanagerUnknown", "Could not read the Alertmanager settings: " + err.Error()
	}
	if len(list.Items) == 0 {
		return "NoAlertmanager", "No Alertmanager runs in " + observability.Namespace + "; the installer sets it up (stage Observability)."
	}
	for _, am := range list.Items {
		if off, _, _ := unstructured.NestedBool(am.Object, "spec", "disableNamespaceMatcher"); !off {
			return "NamespaceMatcher", "Alertmanager " + am.GetName() + " only routes alerts from " + observability.Namespace +
				" to channels. Re-run the installer: it sets disableNamespaceMatcher so project and node alerts reach them."
		}
	}
	return "", ""
}

// ensureSecret creates the channel's empty Secret when missing and returns it.
func (r *NotificationChannelReconciler) ensureSecret(ctx context.Context, ch *kwerftv1.NotificationChannel) (*corev1.Secret, error) {
	key := client.ObjectKey{Namespace: observability.Namespace, Name: observability.ChannelSecret(ch.Name)}
	var sec corev1.Secret
	err := r.reader().Get(ctx, key, &sec)
	if apierrors.IsNotFound(err) {
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: key.Name, Namespace: key.Namespace,
				Labels: map[string]string{LabelManagedBy: ManagedByKwerft, LabelNotificationChannel: ch.Name},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: kwerftv1.GroupVersion.String(), Kind: "NotificationChannel", Name: ch.Name, UID: ch.UID,
					Controller: ptrTo(true), BlockOwnerDeletion: ptrTo(true),
				}},
			},
			Type: corev1.SecretTypeOpaque,
		}
		if err := r.Create(ctx, &sec); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("secret %s appeared meanwhile; retrying", key.Name)
			}
			return nil, err
		}
		log.FromContext(ctx).Info("created notification channel secret", "secret", key.Name)
		return &sec, nil
	}
	if err != nil {
		return nil, err
	}
	if !metav1.IsControlledBy(&sec, ch) {
		// Never adopt credentials: a Secret left by an earlier channel of the
		// same name holds credentials meant for that one.
		if owner := metav1.GetControllerOf(&sec); owner == nil || owner.Kind != "NotificationChannel" || sec.Labels[LabelNotificationChannel] != ch.Name {
			return nil, terminalf("SecretConflict", "The Secret %s/%s exists and does not belong to this channel.", key.Namespace, key.Name)
		}
		if err := r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}); client.IgnoreNotFound(err) != nil {
			return nil, err
		}
		return nil, fmt.Errorf("removed %s left by an earlier channel; recreating it", key.Name)
	}
	return &sec, nil
}

// deleteOrphans removes what a deleted channel left (garbage collection
// does too, but not at once).
func (r *NotificationChannelReconciler) deleteOrphans(ctx context.Context, name string) error {
	var sec corev1.Secret
	err := r.reader().Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: observability.ChannelSecret(name)}, &sec)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	default:
		if owner := metav1.GetControllerOf(&sec); owner != nil && owner.Kind == "NotificationChannel" && owner.Name == name && sec.Labels[LabelNotificationChannel] == name {
			if err := r.Delete(ctx, &sec, client.Preconditions{UID: &sec.UID}); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	var cfg unstructured.Unstructured
	cfg.SetGroupVersionKind(VMAlertmanagerConfigGVK)
	err = r.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: ChannelConfigPrefix + name}, &cfg)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(&cfg); owner == nil || owner.Kind != "NotificationChannel" || owner.Name != name {
		return nil
	}
	uid := cfg.GetUID()
	return client.IgnoreNotFound(r.Delete(ctx, &cfg, client.Preconditions{UID: &uid}))
}

// syncRole lists every channel's Secret in the Role; never an empty
// resourceNames list, which would mean every Secret.
func (r *NotificationChannelReconciler) syncRole(ctx context.Context) error {
	var list kwerftv1.NotificationChannelList
	if err := r.List(ctx, &list); err != nil {
		return err
	}
	var names []string
	for _, ch := range list.Items {
		if ch.DeletionTimestamp.IsZero() {
			names = append(names, observability.ChannelSecret(ch.Name))
		}
	}
	sort.Strings(names)
	role := rbacv1ac.Role(NotifySecretsRole, observability.Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft})
	if len(names) > 0 {
		role = role.WithRules(rbacv1ac.PolicyRule().
			WithAPIGroups("").WithResources("secrets").WithVerbs("patch").WithResourceNames(names...))
	}
	if err := apply(ctx, r.Client, role); err != nil {
		if apierrors.IsNotFound(err) {
			return nil // no observability namespace; reported with the Secret
		}
		return err
	}
	binding := rbacv1ac.RoleBinding(NotifySecretsRole, observability.Namespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithRoleRef(rbacv1ac.RoleRef().WithAPIGroup(rbacv1.GroupName).WithKind("Role").WithName(NotifySecretsRole)).
		WithSubjects(
			rbacv1ac.Subject().WithAPIGroup(rbacv1.GroupName).WithKind(rbacv1.GroupKind).WithName("kwerft:role:owner"),
			rbacv1ac.Subject().WithAPIGroup(rbacv1.GroupName).WithKind(rbacv1.GroupKind).WithName("kwerft:role:admin"),
		)
	return apply(ctx, r.Client, binding)
}

func (r *NotificationChannelReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Rules decide which alerts reach a channel. A rule that stops naming a
	// channel must update that one too, so every channel is looked at.
	allChannels := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, _ client.Object) []reconcile.Request {
		var list kwerftv1.NotificationChannelList
		if err := mgr.GetClient().List(ctx, &list); err != nil {
			return nil
		}
		out := make([]reconcile.Request, 0, len(list.Items))
		for _, ch := range list.Items {
			out = append(out, reconcile.Request{NamespacedName: types.NamespacedName{Name: ch.Name}})
		}
		return out
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("notificationchannel").
		// Status writes (also the console's lastTest) must not trigger a pass;
		// the console marks new credentials with an annotation.
		For(&kwerftv1.NotificationChannel{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&kwerftv1.AlertRule{}, allChannels, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
