// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/observability"
)

// Alert rules. Every AlertRule becomes one group of the VMRule
// "kwerft-alerts" in kwerft-observability, which vmalert (operator-managed,
// selectAllByDefault) evaluates every 20s, or as the group says. Disabled
// rules and rules that do not validate are left out, so one bad Custom
// expression cannot take the others down: the operator rejects a VMRule as a
// whole when any group fails vmalert's validation.
//
// The reconciler also creates the default rules (internal/alerting) when
// they are missing, labelled kwerft.dev/default=true. It never changes an
// existing one; deleting a default rule brings it back with its defaults.
//
// VictoriaMetrics operator objects are unstructured (Kwerft does not import
// the operator's API module) and read uncached; a cluster without the
// operator's CRDs reports every rule as not ready instead of failing.

var (
	VMRuleGVK               = schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMRule"}
	VMAlertmanagerConfigGVK = schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMAlertmanagerConfig"}
	VMAlertmanagerGVK       = schema.GroupVersionKind{Group: "operator.victoriametrics.com", Version: "v1beta1", Kind: "VMAlertmanager"}
)

const (
	// AlertsVMRule is the VMRule holding every AlertRule.
	AlertsVMRule = "kwerft-alerts"
	// vmPoll is how often the reconcilers look at the operator's status
	// until it has seen what they wrote (they do not watch its objects).
	vmPoll = 30 * time.Second
)

var alertRulesRequest = reconcile.Request{NamespacedName: types.NamespacedName{Name: AlertsVMRule}}

// AlertRuleReconciler renders all AlertRules into the VMRule and reports
// each rule's state.
type AlertRuleReconciler struct {
	client.Client
	// ConsoleDomain is the --console-domain flag, used for alert links until
	// ConsoleSettings reports where the console is served.
	ConsoleDomain string
	// NoDefaults skips creating the default rules (tests).
	NoDefaults bool
}

func (r *AlertRuleReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if !r.NoDefaults {
		if err := r.ensureDefaults(ctx); err != nil {
			return ctrl.Result{}, err
		}
	}
	var rules kwerftv1.AlertRuleList
	if err := r.List(ctx, &rules); err != nil {
		return ctrl.Result{}, err
	}
	var channels kwerftv1.NotificationChannelList
	if err := r.List(ctx, &channels); err != nil {
		return ctrl.Result{}, err
	}
	known := map[string]bool{}
	for _, c := range channels.Items {
		if c.DeletionTimestamp.IsZero() {
			known[c.Name] = true
		}
	}
	host := ConsoleHost(ctx, r.Client, r.ConsoleDomain)

	type outcome struct {
		expr            string
		status          metav1.ConditionStatus
		reason, message string
		evaluated       bool
	}
	outcomes := map[string]*outcome{}
	var groups []any
	slices.SortFunc(rules.Items, func(a, b kwerftv1.AlertRule) int { return strings.Compare(a.Name, b.Name) })
	for i := range rules.Items {
		rule := &rules.Items[i]
		if !rule.DeletionTimestamp.IsZero() {
			continue
		}
		o := &outcome{}
		outcomes[rule.Name] = o
		rendered, err := alerting.Render(rule, host)
		if err != nil {
			o.status, o.reason, o.message = metav1.ConditionFalse, "InvalidRule", "Not evaluated: "+err.Error()
			if ferr, ok := err.(*alerting.FieldError); ok && ferr.Field == "expr" {
				o.reason, o.message = "InvalidExpression", "Not evaluated: "+ferr.Message
			}
			continue
		}
		o.expr = rendered.Expr
		if rule.Spec.Disabled {
			o.status, o.reason, o.message = metav1.ConditionTrue, "Disabled", "Disabled; not evaluated."
			continue
		}
		groups = append(groups, vmRuleGroup(rendered))
		o.evaluated = true
		var missing []string
		for _, c := range rule.Spec.Channels {
			if !known[c] {
				missing = append(missing, c)
			}
		}
		switch {
		case len(missing) > 0:
			o.status, o.reason = metav1.ConditionFalse, "ChannelMissing"
			o.message = "Evaluated, but the notification " + plural(len(missing), "channel ", "channels ") + strings.Join(missing, ", ") +
				plural(len(missing), " does", " do") + " not exist; alerts only show in the console."
		case len(rule.Spec.Channels) == 0:
			o.status, o.reason, o.message = metav1.ConditionTrue, "Evaluated", "Evaluated; alerts only show in the console (no channels)."
		default:
			o.status, o.reason, o.message = metav1.ConditionTrue, "Evaluated", "Evaluated; notifies "+strings.Join(rule.Spec.Channels, ", ")+"."
		}
	}

	after, problem, err := r.applyVMRule(ctx, groups)
	if err != nil {
		return ctrl.Result{}, err
	}
	for i := range rules.Items {
		rule := &rules.Items[i]
		o := outcomes[rule.Name]
		if o == nil {
			continue
		}
		if problem != nil && o.evaluated {
			o.status, o.reason, o.message = metav1.ConditionFalse, problem.reason, problem.message
		}
		orig := rule.DeepCopy()
		rule.Status.ObservedGeneration = rule.Generation
		rule.Status.Expr = o.expr
		setReady(&rule.Status.Conditions, rule.Generation, o.status, o.reason, o.message)
		if equality.Semantic.DeepEqual(orig.Status, rule.Status) {
			continue
		}
		if err := patchStatus(ctx, r.Client, rule, orig); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: after}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func vmRuleGroup(r alerting.Rendered) map[string]any {
	rule := map[string]any{
		"alert":       r.Alert,
		"expr":        r.Expr,
		"labels":      stringMap(r.Labels),
		"annotations": stringMap(r.Annotations),
	}
	if r.For > 0 {
		rule["for"] = alerting.FormatDuration(r.For)
	}
	group := map[string]any{"name": r.Alert, "rules": []any{rule}}
	if r.Interval > 0 {
		group["interval"] = alerting.FormatDuration(r.Interval)
	}
	return group
}

func stringMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// stackProblem is why rules that would be evaluated are not.
type stackProblem struct{ reason, message string }

// applyVMRule writes the VMRule and reads what the operator thinks of it.
func (r *AlertRuleReconciler) applyVMRule(ctx context.Context, groups []any) (time.Duration, *stackProblem, error) {
	if groups == nil {
		groups = []any{}
	}
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(VMRuleGVK)
	u.SetName(AlertsVMRule)
	u.SetNamespace(observability.Namespace)
	u.SetLabels(map[string]string{LabelManagedBy: ManagedByKwerft})
	u.Object["spec"] = map[string]any{"groups": groups}
	err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(FieldOwner), client.ForceOwnership)
	switch {
	case meta.IsNoMatchError(err):
		return time.Minute, &stackProblem{"NoMonitoringStack",
			"Not evaluated: the VictoriaMetrics operator is not installed (install.sh stage Observability)."}, nil
	case apierrors.IsNotFound(err):
		return time.Minute, &stackProblem{"NoMonitoringStack",
			"Not evaluated: the namespace " + observability.Namespace + " does not exist (install.sh stage Observability)."}, nil
	case err != nil:
		return 0, nil, err
	}
	var got unstructured.Unstructured
	got.SetGroupVersionKind(VMRuleGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: AlertsVMRule}, &got); err != nil {
		return 0, nil, client.IgnoreNotFound(err)
	}
	observed, _, _ := unstructured.NestedInt64(got.Object, "status", "observedGeneration")
	if observed < got.GetGeneration() {
		return vmPoll, nil, nil
	}
	if msg := operatorRejection(&got); msg != "" {
		return 0, &stackProblem{"RejectedByVMAlert", "vmalert rejected the rules: " + msg}, nil
	}
	return 0, nil, nil
}

// operatorRejection is the error the VictoriaMetrics operator reports for a
// config object (VMRule, VMAlertmanagerConfig) at its current generation:
// status.updateStatus=failed, or per-parent Applied conditions that are
// False (aggregated in status.reason).
func operatorRejection(u *unstructured.Unstructured) string {
	st, _, _ := unstructured.NestedString(u.Object, "status", "updateStatus")
	reason, _, _ := unstructured.NestedString(u.Object, "status", "reason")
	if st == "failed" || reason != "" {
		if reason == "" {
			reason = "no reason given"
		}
		return reason
	}
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if t, _ := m["type"].(string); strings.HasSuffix(t, "/Applied") && m["status"] == "False" {
			msg, _ := m["message"].(string)
			return fmt.Sprint(msg)
		}
	}
	return ""
}

// ensureDefaults creates the default rules that are missing.
func (r *AlertRuleReconciler) ensureDefaults(ctx context.Context) error {
	for _, d := range alerting.DefaultRules() {
		err := r.Get(ctx, client.ObjectKey{Name: d.Name}, &kwerftv1.AlertRule{})
		if err == nil {
			continue
		}
		if !apierrors.IsNotFound(err) {
			return err
		}
		rule := &kwerftv1.AlertRule{
			ObjectMeta: metav1.ObjectMeta{Name: d.Name, Labels: map[string]string{
				observability.LabelDefault: "true", LabelManagedBy: ManagedByKwerft,
			}},
			Spec: d.Spec,
		}
		if err := r.Create(ctx, rule); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		log.FromContext(ctx).Info("created default alert rule", "rule", d.Name)
	}
	return nil
}

func (r *AlertRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{alertRulesRequest}
	})
	// One pass at start, so the defaults appear in a cluster without rules.
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: AlertsVMRule}}}
	return ctrl.NewControllerManagedBy(mgr).
		Named("alertrules").
		WatchesRawSource(source.Channel(start, all)).
		// Status writes must not trigger another pass.
		Watches(&kwerftv1.AlertRule{}, all, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}))).
		// Rules name channels; a channel appearing or going changes their state.
		Watches(&kwerftv1.NotificationChannel{}, all, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(event.UpdateEvent) bool { return false },
		})).
		// Alert links point at the console's hostname.
		Watches(&kwerftv1.ConsoleSettings{}, all).
		Complete(r)
}
