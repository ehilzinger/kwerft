package controllers

import (
	"context"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// UpdatesReconciler is release discovery and the update policy
// (docs/phase6-upgrades.md › Releases, Update policy), in the management
// cluster only: with a policy other than Off it reads the install
// repository every Interval and on "Check now"
// (kwerftv1.AnnotationCheckUpdatesRequested) and writes
// ConsoleSettings.status.updates. With AutoPatch it creates the Upgrade for
// the newest patch release of the running minor when the maintenance
// window is open, as "auto-update". Off makes no outbound request at all.
type UpdatesReconciler struct {
	client.Client
	// Source is the install repository.
	Source upgrades.Source
	// Version is the running Kwerft release.
	Version string
	// Interval between checks (default DefaultUpdateInterval).
	Interval time.Duration
	// Now returns the current time; nil means time.Now.
	Now func() time.Time
}

// DefaultUpdateInterval is how often releases are looked up.
const DefaultUpdateInterval = 6 * time.Hour

func (r *UpdatesReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *UpdatesReconciler) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return DefaultUpdateInterval
}

func (r *UpdatesReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Spec and annotation changes only: the status (written here and by
	// the Domain reconciler) does not start a check.
	changed := predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration() ||
				!equality.Semantic.DeepEqual(e.ObjectOld.GetAnnotations(), e.ObjectNew.GetAnnotations())
		},
	}
	local := predicate.NewPredicateFuncs(func(o client.Object) bool { return o.GetName() == kwerftv1.ConsoleSettingsName })
	return ctrl.NewControllerManagedBy(mgr).
		For(&kwerftv1.ConsoleSettings{}, builder.WithPredicates(local, changed)).
		Named("updates").
		Complete(r)
}

func (r *UpdatesReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	if req.Name != kwerftv1.ConsoleSettingsName {
		return ctrl.Result{}, nil
	}
	var s kwerftv1.ConsoleSettings
	if err := r.Get(ctx, req.NamespacedName, &s); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	orig := s.DeepCopy()
	res, err := r.reconcile(ctx, &s)
	if !equality.Semantic.DeepEqual(orig.Status, s.Status) {
		if perr := patchStatus(ctx, r.Client, &s, orig); perr != nil {
			if apierrors.IsConflict(perr) {
				return ctrl.Result{Requeue: true}, nil
			}
			return ctrl.Result{}, perr
		}
	}
	return res, err
}

// policy returns the effective settings.
func policyOf(s *kwerftv1.ConsoleSettings) (kwerftv1.UpdatePolicy, string, *kwerftv1.UpdateSettings) {
	u := s.Spec.Updates
	if u == nil {
		u = &kwerftv1.UpdateSettings{}
	}
	policy, channel := u.Policy, u.Channel
	if policy == "" {
		policy = kwerftv1.UpdatesNotify
	}
	if channel == "" {
		channel = upgrades.ChannelStable
	}
	return policy, channel, u
}

func (r *UpdatesReconciler) reconcile(ctx context.Context, s *kwerftv1.ConsoleSettings) (ctrl.Result, error) {
	policy, channel, settings := policyOf(s)
	if s.Status.Updates == nil {
		s.Status.Updates = &kwerftv1.UpdatesStatus{}
	}
	st := s.Status.Updates
	if policy == kwerftv1.UpdatesOff {
		// No discovery, no outbound requests, nothing to offer.
		st.Available, st.Error = nil, ""
		return ctrl.Result{}, nil
	}
	if resume := s.Annotations[kwerftv1.AnnotationResumeAutoPatch]; resume != "" && resume == st.AutoPatchPausedBy {
		st.AutoPatchPausedBy = ""
		log.FromContext(ctx).Info("AutoPatch resumed", "after", resume)
	}

	now := r.now()
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return ctrl.Result{}, err
	}
	minors, oldest := kubeletVersions(nodes.Items)
	current := &kwerftv1.UpgradeVersions{Kwerft: r.Version, Kubernetes: oldest}
	if due(st, current, s.Annotations[kwerftv1.AnnotationCheckUpdatesRequested], now, r.interval()) {
		st.CheckedAt = &metav1.Time{Time: now}
		st.Current = current
		d, err := upgrades.Discover(ctx, r.Source, upgrades.Facts{Kwerft: r.Version, Kubernetes: oldest, KubernetesMinors: minors, Channel: channel})
		if err != nil {
			st.Error = err.Error()
			log.FromContext(ctx).Info("release discovery failed", "err", err)
		} else {
			st.Error, st.Available = "", d.Available
		}
	}

	next := st.CheckedAt.Add(r.interval()).Sub(now)
	if policy == kwerftv1.UpdatesAutoPatch && st.AutoPatchPausedBy == "" {
		after, err := r.autoPatch(ctx, s, settings, now)
		if err != nil {
			return ctrl.Result{}, err
		}
		if after > 0 && after < next {
			next = after
		}
	}
	return ctrl.Result{RequeueAfter: max(next, time.Second)}, nil
}

// due: never checked, the interval passed, the running versions changed
// (an upgrade finished), or an owner asked after the last check.
func due(st *kwerftv1.UpdatesStatus, current *kwerftv1.UpgradeVersions, requested string, now time.Time, interval time.Duration) bool {
	if st.CheckedAt == nil || now.Sub(st.CheckedAt.Time) >= interval {
		return true
	}
	if st.Current == nil || *st.Current != *current {
		return true
	}
	if requested == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339, requested)
	return err == nil && at.After(st.CheckedAt.Time)
}

// autoPatch creates the auto-update in an open window and says when to
// look again (the window's end, or its next start).
func (r *UpdatesReconciler) autoPatch(ctx context.Context, s *kwerftv1.ConsoleSettings, settings *kwerftv1.UpdateSettings, now time.Time) (time.Duration, error) {
	w, err := upgrades.ParseWindow(settings.Window)
	if err != nil || w == nil {
		return 0, nil // no (valid) window: AutoPatch never starts anything
	}
	open, end := w.Open(now)
	if !open {
		return w.Next(now).Sub(now), nil
	}
	var list kwerftv1.UpgradeList
	if err := r.List(ctx, &list); err != nil {
		return 0, err
	}
	windowStart := end.Add(-windowLength(settings.Window))
	for _, u := range list.Items {
		if !upgrades.Finished(u.Status.Phase) {
			return time.Minute, nil // one at a time; look again when it is done
		}
	}
	cur, err := upgrades.ParseVersion(r.Version)
	if err != nil {
		return 0, nil
	}
	target := pickAutoPatch(s.Status.Updates.Available, cur, s.Status.Updates.Current, settings.KubernetesPatches)
	if target == nil {
		return end.Sub(now), nil
	}
	for _, u := range list.Items {
		// One attempt per release and window (a cancelled one stays cancelled).
		if auto(&u) && u.Spec.Component == target.Component && u.Spec.Version == target.Version &&
			!u.CreationTimestamp.Time.Before(windowStart) {
			return end.Sub(now), nil
		}
	}
	u := &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: GenerateName(target.Component, target.Version),
			Annotations:  map[string]string{kwerftv1.AnnotationRequestedBy: kwerftv1.RequestedByAutoUpdate},
		},
		Spec: kwerftv1.UpgradeSpec{Component: target.Component, Version: target.Version},
	}
	if err := r.Create(ctx, u); err != nil {
		return 0, err
	}
	log.FromContext(ctx).Info("auto-update created", "upgrade", u.Name, "component", target.Component, "version", target.Version)
	return time.Minute, nil
}

func windowLength(w *kwerftv1.MaintenanceWindow) time.Duration {
	if w != nil && w.Duration != nil && w.Duration.Duration > 0 {
		return w.Duration.Duration
	}
	return upgrades.DefaultWindowDuration
}

// pickAutoPatch: the newest allowed Kwerft patch of the running minor;
// otherwise, with kubernetesPatches, an allowed k3s patch. Never a minor.
func pickAutoPatch(available []kwerftv1.AvailableUpdate, cur upgrades.Version, current *kwerftv1.UpgradeVersions, k3sPatches bool) *kwerftv1.AvailableUpdate {
	var kwerft []kwerftv1.AvailableUpdate
	for _, a := range available {
		v, err := upgrades.ParseVersion(a.Version)
		if err != nil || !a.Allowed || a.Kind != "Patch" {
			continue
		}
		if a.Component == kwerftv1.UpgradeKwerft && cur.Less(v) && cur.SameMinor(v) && v.Pre == "" {
			kwerft = append(kwerft, a)
		}
	}
	if len(kwerft) > 0 {
		slices.SortFunc(kwerft, func(a, b kwerftv1.AvailableUpdate) int {
			return upgrades.MustVersion(b.Version).Compare(upgrades.MustVersion(a.Version))
		})
		return &kwerft[0]
	}
	if !k3sPatches || current == nil {
		return nil
	}
	running, err := upgrades.ParseVersion(current.Kubernetes)
	if err != nil {
		return nil
	}
	for i, a := range available {
		v, err := upgrades.ParseVersion(a.Version)
		if err == nil && a.Component == kwerftv1.UpgradeKubernetes && a.Allowed && a.Kind == "Patch" &&
			running.Less(v) && running.SameMinor(v) && !strings.Contains(a.Version, "rc") {
			return &available[i]
		}
	}
	return nil
}
