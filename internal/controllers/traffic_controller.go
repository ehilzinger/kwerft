package controllers

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// TrafficCounter reports a TrafficRule's Hubble counts (internal/hubble); nil
// when Hubble is off or has not been heard from.
type TrafficCounter interface {
	RuleCounts(rule *kwerftv1.TrafficRule) *kwerftv1.TrafficCounts
}

// countsInterval is how often a rule's status counts are refreshed.
const countsInterval = 5 * time.Minute

// TrafficRuleReconciler renders a TrafficRule into CiliumNetworkPolicies
// (traffic_render.go) and reports them, problems and Hubble's counts in its
// status.
//
// Crossing projects takes consent from the receiving side: a rule in project
// A lets A's apps send to project B's apps, but whether B's apps accept is
// B's decision (its Apps' allowFrom, a TrafficRule in B, or B not being
// isolated). A rule in A never writes a policy in B — a developer of A may
// not be one of B — and its Ready condition says what B still has to allow.
type TrafficRuleReconciler struct {
	client.Client
	// Counts, optional, fills status.counts from Hubble every few minutes.
	Counts TrafficCounter
}

func (r *TrafficRuleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var rule kwerftv1.TrafficRule
	if err := r.Get(ctx, req.NamespacedName, &rule); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !rule.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // policies are garbage-collected through owner references
	}
	orig := rule.DeepCopy()

	ready, err := r.reconcile(ctx, &rule)
	switch {
	case err != nil:
		setReady(&rule.Status.Conditions, rule.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	case ready != nil:
		setReady(&rule.Status.Conditions, rule.Generation, ready.status, ready.reason, ready.message)
	}
	rule.Status.ObservedGeneration = rule.Generation
	if r.Counts != nil && !rule.Spec.Disabled {
		if c := r.Counts.RuleCounts(&rule); c != nil {
			rule.Status.Counts = c
		}
	}
	if !equality.Semantic.DeepEqual(orig.Status, rule.Status) {
		if perr := patchStatus(ctx, r.Client, &rule, orig); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if err != nil && !isTerminal(err) {
		return ctrl.Result{}, err
	}
	if r.Counts != nil {
		return ctrl.Result{RequeueAfter: countsInterval}, nil
	}
	return ctrl.Result{}, nil
}

func (r *TrafficRuleReconciler) reconcile(ctx context.Context, rule *kwerftv1.TrafficRule) (*readiness, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: rule.Namespace}, &ns); err != nil {
		return nil, err
	}
	project := ns.Labels[LabelProject]
	if project == "" {
		return nil, terminalf("NotInProject", "namespace %q is not a Kwerft project", rule.Namespace)
	}
	inName, outName := TrafficPolicyNames(rule.Name)

	if rule.Spec.Disabled {
		if err := r.removePolicies(ctx, rule, inName, outName); err != nil {
			return nil, err
		}
		rule.Status.Policies = nil
		return &readiness{metav1.ConditionTrue, "Disabled", "Disabled: kept, but allows nothing"}, nil
	}
	if err := ValidateTrafficRule(project, &rule.Spec); err != nil {
		// What the rule said before no longer holds; allow nothing rather
		// than something stale.
		if rerr := r.removePolicies(ctx, rule, inName, outName); rerr != nil {
			return nil, rerr
		}
		rule.Status.Policies = nil
		return nil, terminalf("InvalidRule", "%s", err.Error())
	}

	ingress, egress := trafficPolicies(rule, project)
	var applied []string
	for _, p := range []struct {
		name   string
		policy *unstructured.Unstructured
	}{{inName, ingress}, {outName, egress}} {
		if p.policy == nil {
			if err := r.removePolicies(ctx, rule, p.name); err != nil {
				return nil, err
			}
			continue
		}
		err := apply(ctx, r.Client, client.ApplyConfigurationFromUnstructured(p.policy))
		if meta.IsNoMatchError(err) {
			return nil, terminalf("CiliumMissing", "Cilium's CiliumNetworkPolicy is not installed in this cluster; re-run the installer")
		}
		if err != nil {
			return nil, fmt.Errorf("apply policy %s: %w", p.name, err)
		}
		applied = append(applied, p.name)
	}
	rule.Status.Policies = applied

	// Applied. What still keeps traffic from flowing?
	missing, err := r.missingApps(ctx, rule, project)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return &readiness{metav1.ConditionFalse, "AppNotFound",
			"Applied, but there is no app " + strings.Join(missing, ", ") + " yet; the rule takes effect when it is deployed"}, nil
	}
	waiting, err := r.awaitingConsent(ctx, rule, project)
	if err != nil {
		return nil, err
	}
	if len(waiting) > 0 {
		return &readiness{metav1.ConditionFalse, "AwaitingPeer", strings.Join(waiting, "; ")}, nil
	}
	return &readiness{metav1.ConditionTrue, "Applied", DescribeTrafficRule(project, &rule.Spec)}, nil
}

// removePolicies deletes the named policies of rule (only if it owns them).
func (r *TrafficRuleReconciler) removePolicies(ctx context.Context, rule *kwerftv1.TrafficRule, names ...string) error {
	for _, n := range names {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(CiliumNetworkPolicyGVK)
		u.SetName(n)
		u.SetNamespace(rule.Namespace)
		if err := deleteIfControlledBy(ctx, r.Client, u, rule); err != nil {
			return err
		}
	}
	return nil
}

// missingApps lists this project's apps the rule names that do not exist.
// Other projects' apps are not looked up: the status is readable by this
// project's members, who may not be members there.
func (r *TrafficRuleReconciler) missingApps(ctx context.Context, rule *kwerftv1.TrafficRule, project string) ([]string, error) {
	var out []string
	for _, p := range slices.Concat(rule.Spec.From, rule.Spec.To) {
		if p.App == "" {
			continue
		}
		q, app := PeerApp(project, p.App)
		if q != project {
			continue
		}
		err := r.Get(ctx, client.ObjectKey{Namespace: q, Name: app}, &kwerftv1.App{})
		switch {
		case apierrors.IsNotFound(err):
			if !slices.Contains(out, app) {
				out = append(out, app)
			}
		case err != nil:
			return nil, err
		}
	}
	return out, nil
}

// awaitingConsent explains, per other project the rule sends to, what that
// project still has to allow. Empty when every receiver accepts.
func (r *TrafficRuleReconciler) awaitingConsent(ctx context.Context, rule *kwerftv1.TrafficRule, project string) ([]string, error) {
	var out []string
	for _, to := range rule.Spec.To {
		target := to.Project
		app := ""
		if to.App != "" {
			target, app = PeerApp(project, to.App)
		}
		if target == "" || target == project {
			continue // internet, CIDR, or this project's own apps
		}
		ok, err := r.accepts(ctx, target, app, project, rule.Spec.From)
		if err != nil {
			return nil, err
		}
		if !ok {
			what := "its apps"
			if app != "" {
				what = target + "/" + app
			}
			out = append(out, fmt.Sprintf("Project %s does not accept this yet: someone in %s has to add a traffic rule from %s to %s (or turn off its isolation)",
				target, target, describePeers(project, rule.Spec.From), what))
		}
	}
	return out, nil
}

// accepts reports whether project target lets every one of from (apps or
// the project itself in project) reach app (empty: any of its apps).
func (r *TrafficRuleReconciler) accepts(ctx context.Context, target, app, project string, from []kwerftv1.TrafficPeer) (bool, error) {
	var p kwerftv1.Project
	switch err := r.Get(ctx, client.ObjectKey{Name: target}, &p); {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}
	if !projectIsolated(&p) {
		return true, nil
	}
	var rules kwerftv1.TrafficRuleList
	if err := r.List(ctx, &rules, client.InNamespace(target)); err != nil {
		return false, err
	}
	var allowFrom []string
	if app != "" {
		var a kwerftv1.App
		switch err := r.Get(ctx, client.ObjectKey{Namespace: target, Name: app}, &a); {
		case err == nil:
			allowFrom = a.Spec.AllowFrom
		case !apierrors.IsNotFound(err):
			return false, err
		}
	}
	for _, src := range from {
		srcApp := ""
		if src.App != "" {
			_, srcApp = PeerApp(project, src.App)
		}
		if srcApp != "" && slices.Contains(allowFrom, project+"/"+srcApp) {
			continue
		}
		if !slices.ContainsFunc(rules.Items, func(tr kwerftv1.TrafficRule) bool {
			return !tr.Spec.Disabled && admits(&tr, target, app, project, srcApp)
		}) {
			return false, nil
		}
	}
	return true, nil
}

// admits reports whether rule (in project target) lets srcApp of project
// (empty: the whole project) reach app (empty: any app) of target.
func admits(rule *kwerftv1.TrafficRule, target, app, project, srcApp string) bool {
	to := slices.ContainsFunc(rule.Spec.To, func(p kwerftv1.TrafficPeer) bool {
		if p.Project == target {
			return true
		}
		if p.App == "" {
			return false
		}
		q, a := PeerApp(target, p.App)
		return q == target && app != "" && a == app
	})
	from := slices.ContainsFunc(rule.Spec.From, func(p kwerftv1.TrafficPeer) bool {
		if p.Project == project {
			return true
		}
		if p.App == "" || srcApp == "" {
			return false
		}
		q, a := PeerApp(target, p.App)
		return q == project && a == srcApp
	})
	return to && from
}

// DescribeTrafficRule says what a rule allows (the Ready message, audit).
func DescribeTrafficRule(project string, spec *kwerftv1.TrafficRuleSpec) string {
	ports := "every port"
	if len(spec.Ports) > 0 {
		var ps []string
		for _, p := range spec.Ports {
			s := fmt.Sprintf("%s %d", cmp.Or(p.Protocol, "TCP"), p.Port)
			if p.EndPort > p.Port {
				s += fmt.Sprintf("-%d", p.EndPort)
			}
			ps = append(ps, s)
		}
		ports = strings.Join(ps, ", ")
	}
	return fmt.Sprintf("Allows %s to %s on %s", describePeers(project, spec.From), describePeers(project, spec.To), ports)
}

func describePeers(project string, peers []kwerftv1.TrafficPeer) string {
	var out []string
	for _, p := range peers {
		switch {
		case p.App != "":
			q, a := PeerApp(project, p.App)
			if q == project {
				out = append(out, a)
			} else {
				out = append(out, q+"/"+a)
			}
		case p.Project != "":
			out = append(out, "project "+p.Project)
		case p.Internet:
			out = append(out, "the internet")
		case p.CIDR != "":
			out = append(out, p.CIDR)
		}
	}
	return strings.Join(out, ", ")
}

// rulesFor enqueues the TrafficRules a change in namespace may concern: the
// namespace's own (its apps appeared or went) and every rule elsewhere that
// sends to it (its consent changed).
func (r *TrafficRuleReconciler) rulesFor(ctx context.Context, namespace string) []reconcile.Request {
	var rules kwerftv1.TrafficRuleList
	if err := r.List(ctx, &rules); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, tr := range rules.Items {
		concerned := tr.Namespace == namespace
		for _, p := range tr.Spec.To {
			q := p.Project
			if p.App != "" {
				q, _ = PeerApp(tr.Namespace, p.App)
			}
			concerned = concerned || q == namespace
		}
		if concerned {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&tr)})
		}
	}
	return reqs
}

func (r *TrafficRuleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	policy := &metav1.PartialObjectMetadata{}
	policy.SetGroupVersionKind(CiliumNetworkPolicyGVK)
	byNamespace := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		return r.rulesFor(ctx, o.GetNamespace())
	})
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes (counts) must not re-trigger.
		For(&kwerftv1.TrafficRule{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(policy, builder.OnlyMetadata).
		// Another project's rule is its consent; apps appearing make
		// "no app yet" go away.
		Watches(&kwerftv1.TrafficRule{}, byNamespace, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&kwerftv1.App{}, byNamespace, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&kwerftv1.Project{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
			return r.rulesFor(ctx, o.GetName())
		}), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("trafficrule").
		Complete(r)
}
