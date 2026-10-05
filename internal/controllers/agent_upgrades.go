package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// Agent clusters' Kwerft upgrades (docs/phase6-upgrades.md › Agent
// clusters, W6). The console upgrades itself first, then each connected
// agent cluster, one after another:
//
//  1. The API (U5), as the owner, creates the console's Upgrade and, in
//     every connected agent cluster through its tunnel and still as the
//     owner, a held Upgrade (FleetMember): label kwerft.dev/fleet,
//     annotations kwerft.dev/hold, kwerft.dev/fleet-order and
//     kwerft.dev/after-upgrade. The agent's Upgrade controller keeps it
//     Queued with the hold as its message, without holding others there.
//  2. AgentUpgradesReconciler (here, the console) watches those Upgrades
//     in every connected cluster. Once the console's Upgrade Succeeded it
//     releases one member (removes the hold) and waits until it finished
//     before the next. If the console's Upgrade ended otherwise, it
//     cancels the held members. Releasing and cancelling are the
//     controller's own doing on objects the owner created, under Kwerft's
//     identity there.
//  3. Disconnected clusters are skipped (their members wait, and go when
//     they are back); clusters the API skipped have no member.
//
// The console's Upgrade gets the condition AgentClusters that sums the
// members up.

// ConditionAgentClusters on the console's Upgrade of an "Upgrade all".
const ConditionAgentClusters = "AgentClusters"

// FleetMember is the Upgrade the API creates in an agent cluster, as the
// owner, for one member of an "Upgrade all": generateName as GenerateName,
// held until the console releases it. fleet names the fleet (after, or
// upgrades.NewFleetID() when the console needs no upgrade); after is the
// console's own Upgrade ("" when there is none); order is its place.
func FleetMember(fleet, after string, order int, version, requestedBy string) *kwerftv1.Upgrade {
	hold := "Waiting for its turn after the other agent clusters."
	if after != "" {
		hold = "Waiting for the console's upgrade " + after + "."
	}
	annotations := map[string]string{
		kwerftv1.AnnotationRequestedBy: requestedBy,
		kwerftv1.AnnotationHold:        hold,
		kwerftv1.AnnotationFleetOrder:  strconv.Itoa(order),
	}
	if after != "" {
		annotations[kwerftv1.AnnotationAfterUpgrade] = after
	}
	return &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: GenerateName(kwerftv1.UpgradeKwerft, version),
			Labels:       map[string]string{kwerftv1.LabelFleet: fleet},
			Annotations:  annotations,
		},
		Spec: kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: version},
	}
}

// AgentUpgradesReconciler starts the members of agent-cluster fleets one
// after another (above). It runs in the console only.
type AgentUpgradesReconciler struct {
	// Client is the management cluster's (Clusters, the console's Upgrades).
	Client client.Client
	// Clusters reaches each agent cluster with Kwerft's identity there.
	Clusters ClusterClients
	// Version is the console's running release: no member is started past
	// it.
	Version string
	// Interval between looks at the agent clusters while a fleet is
	// unfinished (default 30 s); they cannot be watched from here.
	Interval time.Duration
}

// fleetKey is the one request this reconciler handles: all fleets at once.
var fleetKey = reconcile.Request{NamespacedName: types.NamespacedName{Name: "fleets"}}

func (r *AgentUpgradesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	one := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{fleetKey}
	})
	return ctrl.NewControllerManagedBy(mgr).
		Named("agent-upgrades").
		Watches(&kwerftv1.Upgrade{}, one).
		Watches(&kwerftv1.Cluster{}, one).
		Complete(r)
}

// fleetMember is one member as seen in its cluster.
type fleetMember struct {
	cluster string
	c       client.Client
	u       kwerftv1.Upgrade
}

func (m *fleetMember) order() int {
	n, err := strconv.Atoi(m.u.Annotations[kwerftv1.AnnotationFleetOrder])
	if err != nil {
		return 1 << 30
	}
	return n
}

func (m *fleetMember) held() bool { return m.u.Annotations[kwerftv1.AnnotationHold] != "" }

func (m *fleetMember) finished() bool { return upgrades.Finished(m.u.Status.Phase) }

func (r *AgentUpgradesReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	var list kwerftv1.ClusterList
	if err := r.Client.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	fleets := map[string][]*fleetMember{}
	agentVersion := map[string]string{}
	for _, cl := range list.Items {
		if cl.Name == clusters.Local || cl.Status.Phase != ClusterConnected {
			continue
		}
		agentVersion[cl.Name] = cl.Status.AgentVersion
		c, err := r.Clusters.For(ctx, cl.Name)
		if err != nil {
			log.FromContext(ctx).Info("agent upgrades: cluster skipped", "cluster", cl.Name, "err", err)
			continue
		}
		var ups kwerftv1.UpgradeList
		if err := c.List(ctx, &ups, client.HasLabels{kwerftv1.LabelFleet}); err != nil {
			log.FromContext(ctx).Info("agent upgrades: cannot list upgrades", "cluster", cl.Name, "err", err)
			continue
		}
		for _, u := range ups.Items {
			fleets[u.Labels[kwerftv1.LabelFleet]] = append(fleets[u.Labels[kwerftv1.LabelFleet]], &fleetMember{cluster: cl.Name, c: c, u: u})
		}
	}
	pending := false
	for fleet, members := range fleets {
		slices.SortFunc(members, func(a, b *fleetMember) int {
			if d := a.order() - b.order(); d != 0 {
				return d
			}
			return strings.Compare(a.cluster, b.cluster)
		})
		more, err := r.fleet(ctx, fleet, members, agentVersion)
		if err != nil {
			return ctrl.Result{}, err
		}
		pending = pending || more
	}
	if pending {
		return ctrl.Result{RequeueAfter: r.interval()}, nil
	}
	return ctrl.Result{}, nil
}

func (r *AgentUpgradesReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return 30 * time.Second
}

// fleet moves one fleet on; more reports that it is not done.
func (r *AgentUpgradesReconciler) fleet(ctx context.Context, fleet string, members []*fleetMember, agentVersion map[string]string) (bool, error) {
	after := ""
	for _, m := range members {
		after = max(after, m.u.Annotations[kwerftv1.AnnotationAfterUpgrade])
	}
	var console *kwerftv1.Upgrade
	if after != "" {
		var u kwerftv1.Upgrade
		err := r.Client.Get(ctx, client.ObjectKey{Name: after}, &u)
		switch {
		case apierrors.IsNotFound(err):
			if err := r.cancelHeld(ctx, members, "kwerft (the console's upgrade "+after+" is gone)"); err != nil {
				return false, err
			}
			return r.unfinished(members), nil
		case err != nil:
			return false, err
		}
		console = &u
		defer func() { r.summarize(ctx, console, members) }()
		switch p := u.Status.Phase; {
		case !upgrades.Finished(p):
			return r.unfinished(members), nil
		case p != kwerftv1.UpgradeSucceeded:
			if err := r.cancelHeld(ctx, members, fmt.Sprintf("kwerft (the console's upgrade %s ended %s)", after, p)); err != nil {
				return false, err
			}
			return r.unfinished(members), nil
		}
	}
	var next *fleetMember
	for _, m := range members {
		switch {
		case m.finished():
		case !m.held():
			return true, nil // one at a time: this one runs
		case next == nil:
			next = m
		}
	}
	if next == nil {
		return false, nil
	}
	if err := upgrades.AgentTargetAllowed(r.Version, agentVersion[next.cluster], next.u.Spec.Version); err != nil {
		return true, r.annotate(ctx, next, map[string]any{kwerftv1.AnnotationCancelRequested: "kwerft (" + err.Error() + ")"})
	}
	log.FromContext(ctx).Info("agent upgrade released", "fleet", fleet, "cluster", next.cluster, "upgrade", next.u.Name)
	if err := r.annotate(ctx, next, map[string]any{kwerftv1.AnnotationHold: nil}); err != nil {
		return true, err
	}
	// The others wait for their turn now, not for the console.
	for _, m := range members {
		if m != next && m.held() && !m.finished() {
			msg := "Waiting for its turn: " + next.cluster + " upgrades first."
			if m.u.Annotations[kwerftv1.AnnotationHold] != msg {
				if err := r.annotate(ctx, m, map[string]any{kwerftv1.AnnotationHold: msg}); err != nil {
					return true, err
				}
			}
		}
	}
	return true, nil
}

func (r *AgentUpgradesReconciler) unfinished(members []*fleetMember) bool {
	return slices.ContainsFunc(members, func(m *fleetMember) bool { return !m.finished() })
}

// cancelHeld cancels the members not started yet (the agent's controller
// honours the annotation before anything changes).
func (r *AgentUpgradesReconciler) cancelHeld(ctx context.Context, members []*fleetMember, by string) error {
	for _, m := range members {
		if m.held() && !m.finished() && m.u.Annotations[kwerftv1.AnnotationCancelRequested] == "" {
			if err := r.annotate(ctx, m, map[string]any{kwerftv1.AnnotationCancelRequested: by}); err != nil {
				return err
			}
		}
	}
	return nil
}

// annotate merge-patches annotations (nil removes one) of a member in its
// cluster.
func (r *AgentUpgradesReconciler) annotate(ctx context.Context, m *fleetMember, annotations map[string]any) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": annotations}})
	err := m.c.Patch(ctx, &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: m.u.Name}}, client.RawPatch(types.MergePatchType, patch))
	if err == nil {
		if m.u.Annotations == nil {
			m.u.Annotations = map[string]string{}
		}
		for k, v := range annotations {
			if v == nil {
				delete(m.u.Annotations, k)
			} else {
				m.u.Annotations[k] = v.(string)
			}
		}
	}
	return client.IgnoreNotFound(err)
}

// summarize sets the console Upgrade's AgentClusters condition, best
// effort.
func (r *AgentUpgradesReconciler) summarize(ctx context.Context, console *kwerftv1.Upgrade, members []*fleetMember) {
	var parts []string
	done := true
	for _, m := range members {
		state := string(m.u.Status.Phase)
		switch {
		case m.held() && !m.finished():
			state = "waiting"
		case state == "":
			state = "starting"
		}
		done = done && m.finished()
		parts = append(parts, m.cluster+": "+state)
	}
	cond := metav1.Condition{Type: ConditionAgentClusters, Status: metav1.ConditionFalse, Reason: "Upgrading",
		Message: "Agent clusters (" + strings.Join(parts, ", ") + ").", ObservedGeneration: console.Generation}
	if done {
		cond.Status, cond.Reason = metav1.ConditionTrue, "Finished"
	}
	orig := console.DeepCopy()
	meta.SetStatusCondition(&console.Status.Conditions, cond)
	if equality.Semantic.DeepEqual(orig.Status, console.Status) {
		return
	}
	if err := patchStatus(ctx, r.Client, console, orig); err != nil {
		log.FromContext(ctx).Info("agent upgrades: cannot update the console's upgrade", "upgrade", console.Name, "err", err)
	}
}
