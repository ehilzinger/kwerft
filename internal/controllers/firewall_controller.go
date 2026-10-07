// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/firewall"
)

// FirewallReconciler keeps the installer's base rules as required
// FirewallRules, validates the rest, and renders all of them into the
// desired-state ConfigMap the node agents apply (internal/firewall). It
// reads the agents' reports back into each rule's status, and records the
// rules as confirmed once every node runs them without a pending change, so
// "Roll back now" can restore them.
//
// It never applies anything to a node itself, and it never confirms a change
// on a user's behalf: confirmation is the annotation an owner or admin writes
// through the console (impersonated) on the required rule "ssh".
type FirewallReconciler struct {
	client.Client
	// APIReader reads the ConfigMaps uncached (the manager does not cache
	// ConfigMaps; only these two matter here).
	APIReader client.Reader
	// PrivateNetwork is the cluster's private network (install.sh), shown in
	// the required rule cluster-private; empty without one.
	PrivateNetwork string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

var firewallRequest = reconcile.Request{NamespacedName: client.ObjectKey{Name: "firewall"}}

// agentStale: a node whose agent has not reported for this long counts as
// not running.
const agentStale = 2 * time.Minute

func (r *FirewallReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// FirewallSnapshot is the rule set as last confirmed (SnapshotKey).
type FirewallSnapshot struct {
	Revision string                  `json:"revision"`
	Rules    []FirewallSnapshotEntry `json:"rules"`
}

// FirewallSnapshotEntry is one rule of a snapshot.
type FirewallSnapshotEntry struct {
	Name     string                    `json:"name"`
	Required bool                      `json:"required,omitempty"`
	Spec     kwerftv1.FirewallRuleSpec `json:"spec"`
}

func (r *FirewallReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	if err := r.ensureRequired(ctx); err != nil {
		return ctrl.Result{}, err
	}
	var rules kwerftv1.FirewallRuleList
	if err := r.List(ctx, &rules); err != nil {
		return ctrl.Result{}, err
	}
	var nodeList corev1.NodeList
	if err := r.List(ctx, &nodeList); err != nil {
		return ctrl.Result{}, err
	}
	nodes := make([]firewall.Node, 0, len(nodeList.Items))
	for _, n := range nodeList.Items {
		_, cp := n.Labels["node-role.kubernetes.io/control-plane"]
		nodes = append(nodes, firewall.Node{Name: n.Name, ControlPlane: cp})
	}
	var anchor *kwerftv1.FirewallRule
	for i := range rules.Items {
		if rules.Items[i].Name == firewall.RuleSSH {
			anchor = &rules.Items[i]
		}
	}
	attempt, confirmedByUser := "", ""
	if anchor != nil {
		attempt, confirmedByUser = anchor.Annotations[firewall.AnnotationAttempt], anchor.Annotations[firewall.AnnotationConfirmed]
	}
	desired, invalid := firewall.Build(rules.Items, nodes, attempt)

	cm, err := r.desiredConfigMap(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	var previous firewall.Desired
	if raw := cm.Data[firewall.DesiredKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &previous)
	}
	var snapshot FirewallSnapshot
	if raw := cm.Data[firewall.SnapshotKey]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &snapshot)
	}
	statuses, err := r.nodeStatuses(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	agg := AggregateFirewall(desired, statuses, r.now())

	// Confirmed: an owner's or admin's confirmation of this revision (unless
	// a node rolled it back already), or every running agent took it without
	// a pending change (it took nothing away).
	desired.Confirmed = previous.Confirmed
	switch {
	case confirmedByUser == desired.Revision && agg.RolledBack == 0 && agg.Failed == 0:
		desired.Confirmed = desired.Revision
	case agg.Reporting > 0 && agg.InSync == agg.Reporting:
		desired.Confirmed = desired.Revision
	}
	if desired.Confirmed == desired.Revision && snapshot.Revision != desired.Revision {
		snapshot = FirewallSnapshot{Revision: desired.Revision}
		for _, rule := range rules.Items {
			if rule.DeletionTimestamp.IsZero() {
				snapshot.Rules = append(snapshot.Rules, FirewallSnapshotEntry{Name: rule.Name, Required: firewall.IsRequired(&rule), Spec: rule.Spec})
			}
		}
		slices.SortFunc(snapshot.Rules, func(a, b FirewallSnapshotEntry) int { return strings.Compare(a.Name, b.Name) })
	}
	if err := r.writeDesired(ctx, cm, desired, snapshot); err != nil {
		return ctrl.Result{}, err
	}

	for i := range rules.Items {
		rule := &rules.Items[i]
		if !rule.DeletionTimestamp.IsZero() {
			continue
		}
		status, reason, message := ruleCondition(rule, invalid[rule.Name], agg)
		orig := rule.DeepCopy()
		rule.Status.ObservedGeneration = rule.Generation
		setReady(&rule.Status.Conditions, rule.Generation, status, reason, message)
		if equality.Semantic.DeepEqual(orig.Status, rule.Status) {
			continue
		}
		if err := patchStatus(ctx, r.Client, rule, orig); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}
	// Reports arrive through the watch; this only notices agents that
	// stopped reporting.
	return ctrl.Result{RequeueAfter: agentStale / 2}, nil
}

// ensureRequired creates the base rules that are missing and puts back the
// fixed parts of those that were edited (only the SSH rule's sources change).
func (r *FirewallReconciler) ensureRequired(ctx context.Context) error {
	for _, want := range firewall.Required(r.PrivateNetwork) {
		var have kwerftv1.FirewallRule
		err := r.Get(ctx, client.ObjectKey{Name: want.Name}, &have)
		if apierrors.IsNotFound(err) {
			if err := r.Create(ctx, &want); err != nil && !apierrors.IsAlreadyExists(err) {
				return err
			}
			log.FromContext(ctx).Info("created required firewall rule", "rule", want.Name)
			continue
		}
		if err != nil {
			return err
		}
		spec, _ := firewall.RequiredSpec(want.Name, r.PrivateNetwork, have.Spec)
		if have.Labels[firewall.LabelRequired] == "true" && equality.Semantic.DeepEqual(have.Spec, spec) {
			continue
		}
		if have.Labels == nil {
			have.Labels = map[string]string{}
		}
		for k, v := range want.Labels {
			have.Labels[k] = v
		}
		have.Spec = spec
		if err := r.Update(ctx, &have); err != nil && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

func (r *FirewallReconciler) desiredConfigMap(ctx context.Context) (*corev1.ConfigMap, error) {
	var cm corev1.ConfigMap
	err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap,
			Labels: map[string]string{LabelManagedBy: ManagedByKwerft}}}
		return &cm, nil
	}
	return &cm, err
}

func (r *FirewallReconciler) writeDesired(ctx context.Context, cm *corev1.ConfigMap, d firewall.Desired, snap FirewallSnapshot) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	data := map[string]string{firewall.DesiredKey: string(raw)}
	if snap.Revision != "" {
		s, err := json.Marshal(snap)
		if err != nil {
			return err
		}
		data[firewall.SnapshotKey] = string(s)
	}
	if cm.ResourceVersion == "" {
		cm.Data = data
		if err := r.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	} else if !equality.Semantic.DeepEqual(cm.Data, data) {
		cm.Data = data
		if err := r.Update(ctx, cm); err != nil {
			return err
		}
	}
	// The agents may only patch the status ConfigMap, so it must exist.
	status := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap,
		Labels: map[string]string{LabelManagedBy: ManagedByKwerft}}}
	if err := r.Create(ctx, status); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// nodeStatuses reads what the agents reported, by node.
func (r *FirewallReconciler) nodeStatuses(ctx context.Context) (map[string]firewall.NodeStatus, error) {
	return ReadFirewallStatus(ctx, r.APIReader)
}

// ReadFirewallStatus reads the agents' reports from the status ConfigMap
// (missing: none yet). The console reads it too.
func ReadFirewallStatus(ctx context.Context, c client.Reader) (map[string]firewall.NodeStatus, error) {
	var cm corev1.ConfigMap
	err := c.Get(ctx, client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.StatusConfigMap}, &cm)
	if apierrors.IsNotFound(err) {
		return map[string]firewall.NodeStatus{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make(map[string]firewall.NodeStatus, len(cm.Data))
	for node, raw := range cm.Data {
		var s firewall.NodeStatus
		if json.Unmarshal([]byte(raw), &s) == nil {
			out[node] = s
		}
	}
	return out, nil
}

// FirewallProgress is where the nodes are with the desired revision.
type FirewallProgress struct {
	Revision string
	// Counts of the nodes in the desired state whose agent reported
	// recently (reporting), by what they report for this revision.
	Reporting, InSync, Pending, RolledBack, Failed, Waiting, Other int
	// Deadline is the earliest rollback of a pending change.
	Deadline *time.Time
	// Problems are the nodes that do not run the rules, with why.
	Problems []string
	Nodes    int
}

// FirewallNodeState is one node's state for the desired revision.
func FirewallNodeState(rev string, s firewall.NodeStatus, ok bool, now time.Time) string {
	switch {
	case !ok:
		return "no-agent"
	case now.Sub(s.UpdatedAt) > agentStale:
		return "stale"
	case s.State == firewall.StatePaused || s.State == firewall.StateUnprepared:
		return s.State
	case s.Seen != rev:
		return "waiting"
	case s.State == firewall.StatePending && s.Pending == rev:
		return firewall.StatePending
	case s.State == firewall.StateRolledBack && s.RolledBack == rev:
		return firewall.StateRolledBack
	case s.State == firewall.StateError:
		return firewall.StateError
	case s.State == firewall.StateInSync && s.Confirmed == rev:
		return firewall.StateInSync
	}
	return "waiting"
}

// AggregateFirewall summarises the agents' reports for the desired revision.
func AggregateFirewall(d firewall.Desired, statuses map[string]firewall.NodeStatus, now time.Time) FirewallProgress {
	p := FirewallProgress{Revision: d.Revision, Nodes: len(d.Nodes)}
	names := make([]string, 0, len(d.Nodes))
	for n := range d.Nodes {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		s, ok := statuses[n]
		state := FirewallNodeState(d.Revision, s, ok, now)
		if state != "no-agent" && state != "stale" {
			p.Reporting++
		}
		switch state {
		case firewall.StateInSync:
			p.InSync++
		case firewall.StatePending:
			p.Pending++
			if s.Deadline != nil && (p.Deadline == nil || s.Deadline.Before(*p.Deadline)) {
				p.Deadline = s.Deadline
			}
		case firewall.StateRolledBack:
			p.RolledBack++
			p.Problems = append(p.Problems, n+": rolled back")
		case firewall.StateError:
			p.Failed++
			p.Problems = append(p.Problems, n+": "+s.Message)
		case "waiting":
			p.Waiting++
		case "no-agent":
			p.Other++
			p.Problems = append(p.Problems, n+": no node agent has reported")
		case "stale":
			p.Other++
			p.Problems = append(p.Problems, n+": the node agent stopped reporting")
		default:
			p.Other++
			p.Problems = append(p.Problems, n+": "+s.Message)
		}
	}
	return p
}

// ruleCondition is a rule's Ready condition.
func ruleCondition(rule *kwerftv1.FirewallRule, invalid *firewall.FieldError, p FirewallProgress) (metav1.ConditionStatus, string, string) {
	switch {
	case invalid != nil:
		return metav1.ConditionFalse, "Invalid", "Not applied: " + invalid.Message
	case firewall.IsRequired(rule) && (rule.Name != firewall.RuleSSH || len(rule.Spec.Sources) == 0):
		return metav1.ConditionTrue, "Baseline", "Part of the installer's firewall on every node."
	case rule.Spec.Disabled:
		return metav1.ConditionTrue, "Disabled", "Disabled; not applied."
	case p.Nodes == 0:
		return metav1.ConditionFalse, "NoNodes", "No nodes yet."
	case p.RolledBack > 0:
		return metav1.ConditionFalse, "RolledBack", "Rolled back because the change was not confirmed in time (" + strings.Join(p.Problems, "; ") + ")."
	case p.Failed > 0:
		return metav1.ConditionFalse, "Failed", strings.Join(p.Problems, "; ")
	case p.Pending > 0:
		msg := fmt.Sprintf("Applied on %d of %d nodes, waiting for confirmation in the console.", p.Pending+p.InSync, p.Nodes)
		if p.Deadline != nil {
			msg = fmt.Sprintf("Applied on %d of %d nodes; rolls back at %s unless confirmed in the console.", p.Pending+p.InSync, p.Nodes, p.Deadline.UTC().Format(time.TimeOnly+" MST"))
		}
		return metav1.ConditionFalse, "PendingConfirmation", msg
	case p.InSync == p.Nodes:
		return metav1.ConditionTrue, "Applied", fmt.Sprintf("Applied on %s.", plural(p.Nodes, "the node", fmt.Sprintf("all %d nodes", p.Nodes)))
	case len(p.Problems) > 0:
		return metav1.ConditionFalse, "NotApplied", fmt.Sprintf("Applied on %d of %d nodes. %s", p.InSync, p.Nodes, strings.Join(p.Problems, "; "))
	}
	return metav1.ConditionFalse, "Applying", fmt.Sprintf("Applying: %d of %d nodes done.", p.InSync, p.Nodes)
}

func (r *FirewallReconciler) SetupWithManager(mgr ctrl.Manager) error {
	all := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{firewallRequest}
	})
	// One pass at start: the required rules appear in a new cluster.
	start := make(chan event.GenericEvent, 1)
	start <- event.GenericEvent{Object: &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: firewall.RuleSSH}}}
	// The agents' reports: an informer of its own for exactly the status
	// ConfigMap, so the manager does not cache every ConfigMap.
	reports, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(), HTTPClient: mgr.GetHTTPClient(),
		ByObject: map[client.Object]cache.ByObject{&corev1.ConfigMap{}: {
			Namespaces: map[string]cache.Config{firewall.Namespace: {}},
			Field:      fields.OneTermEqualSelector("metadata.name", firewall.StatusConfigMap),
		}},
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(reports); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		Named("firewall").
		WatchesRawSource(source.Channel(start, all)).
		WatchesRawSource(source.Kind(reports, client.Object(&corev1.ConfigMap{}), all)).
		// Status writes must not trigger another pass; confirmations
		// (annotations) and the required label must.
		Watches(&kwerftv1.FirewallRule{}, all, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.LabelChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		// Nodes joining, leaving or becoming control-plane nodes.
		Watches(&corev1.Node{}, all, builder.WithPredicates(predicate.LabelChangedPredicate{})).
		Complete(r)
}
