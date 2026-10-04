package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const maxHistory = 20

// AppReconciler renders an App into a Deployment (or a StatefulSet when it
// has disks of its own; shared Volumes alone keep it a Deployment), a
// Service, HTTPRoutes for public ports and a NetworkPolicy, and reports
// rollout progress and revisions in App.status.
type AppReconciler struct {
	client.Client
	// Registry tags the images of Git apps' revisions so the registry's
	// retention keeps them; nil leaves the registry alone.
	Registry *RegistryKeeper
}

func (r *AppReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var app kwerftv1.App
	if err := r.Get(ctx, req.NamespacedName, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !app.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // children are garbage-collected through owner references
	}
	orig := app.DeepCopy()

	ready, err := r.reconcile(ctx, &app)
	switch {
	case err != nil:
		setReady(&app.Status.Conditions, app.Generation, metav1.ConditionFalse, reasonOf(err), err.Error())
	case ready != nil:
		setReady(&app.Status.Conditions, app.Generation, ready.status, ready.reason, ready.message)
	}
	app.Status.ObservedGeneration = app.Generation
	if !equality.Semantic.DeepEqual(orig.Status, app.Status) {
		if perr := r.Status().Patch(ctx, &app, client.MergeFrom(orig)); perr != nil {
			return ctrl.Result{}, perr
		}
	}
	if isTerminal(err) {
		return ctrl.Result{}, nil
	}
	if err == nil && r.Registry != nil && app.Spec.Source.Git != nil {
		if kerr := r.Registry.Keep(ctx, app.Namespace, app.Name, app.Status.History); kerr != nil {
			// The rollout is done; only the protection of older images is
			// missing. Retry without holding anything up.
			log.FromContext(ctx).Error(kerr, "cannot protect the App's images in the registry")
			return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
		}
	}
	return ctrl.Result{}, err
}

type readiness struct {
	status  metav1.ConditionStatus
	reason  string
	message string
}

func (r *AppReconciler) reconcile(ctx context.Context, app *kwerftv1.App) (*readiness, error) {
	var ns corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: app.Namespace}, &ns); err != nil {
		return nil, err
	}
	project := ns.Labels[LabelProject]
	if project == "" {
		return nil, terminalf("NotInProject", "namespace %q is not a Kwerft project; create a Project first", app.Namespace)
	}

	resolved, err := resolveImage(ctx, r.Client, app)
	if err != nil {
		return nil, err
	}
	if resolved == nil {
		return &readiness{metav1.ConditionFalse, "AwaitingBuild",
			"Waiting for the first successful build of " + app.Spec.Source.Git.Repository}, nil
	}
	image := resolved.image

	missing, err := missingVolumes(ctx, r.Client, app.Namespace, app.Spec.Volumes)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return &readiness{metav1.ConditionFalse, "VolumeNotFound", "Waiting for Volume " + strings.Join(missing, ", ")}, nil
	}

	rd := newAppRender(app, image, project)

	// Workload: exactly one of Deployment or StatefulSet.
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	if rd.stateful() {
		if err := apply(ctx, r.Client, rd.statefulSet()); err != nil {
			return nil, fmt.Errorf("apply statefulset: %w", err)
		}
		if err := deleteIfControlledBy(ctx, r.Client, deploy, app); err != nil {
			return nil, err
		}
	} else {
		if err := apply(ctx, r.Client, rd.deployment()); err != nil {
			return nil, fmt.Errorf("apply deployment: %w", err)
		}
		if err := deleteIfControlledBy(ctx, r.Client, sts, app); err != nil {
			return nil, err
		}
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: app.Name, Namespace: app.Namespace}}
	if len(app.Spec.Ports) > 0 {
		if err := apply(ctx, r.Client, rd.service()); err != nil {
			return nil, fmt.Errorf("apply service: %w", err)
		}
	} else if err := deleteIfControlledBy(ctx, r.Client, svc, app); err != nil {
		return nil, err
	}

	if err := r.reconcileDomains(ctx, app, rd); err != nil {
		return nil, err
	}
	if err := r.reconcileRoutes(ctx, app, rd); err != nil {
		return nil, err
	}

	if err := apply(ctx, r.Client, rd.networkPolicy()); err != nil {
		return nil, fmt.Errorf("apply network policy: %w", err)
	}

	// Revisions: a new one whenever the spec or the resolved image changes.
	if h := app.Status.History; len(h) == 0 || h[0].Image != image || h[0].Generation != app.Generation {
		app.Status.Revision++
		rev := kwerftv1.AppRevision{Number: app.Status.Revision, Image: image, Generation: app.Generation,
			Build: resolved.build, Commit: resolved.commit, Time: metav1.Now()}
		app.Status.History = append([]kwerftv1.AppRevision{rev}, app.Status.History...)
		if len(app.Status.History) > maxHistory {
			app.Status.History = app.Status.History[:maxHistory]
		}
	}
	app.Status.Image = image
	app.Status.URLs = rd.urls()

	// Rollout progress from the workload we just applied.
	var readyReplicas, updated int32
	if rd.stateful() {
		if err := r.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
			return nil, err
		}
		readyReplicas, updated = sts.Status.ReadyReplicas, sts.Status.UpdatedReplicas
	} else {
		if err := r.Get(ctx, client.ObjectKeyFromObject(deploy), deploy); err != nil {
			return nil, err
		}
		readyReplicas, updated = deploy.Status.ReadyReplicas, deploy.Status.UpdatedReplicas
	}
	app.Status.ReadyReplicas = readyReplicas

	want := rd.replicas()
	switch {
	case want == 0:
		return &readiness{metav1.ConditionTrue, "ScaledToZero", "Scaled to zero replicas"}, nil
	case readyReplicas >= want && updated >= want:
		return &readiness{metav1.ConditionTrue, "Available", fmt.Sprintf("%d/%d replicas ready", readyReplicas, want)}, nil
	default:
		return &readiness{metav1.ConditionFalse, "Progressing", fmt.Sprintf("%d/%d replicas ready", readyReplicas, want)}, nil
	}
}

// reconcileDomains claims a Domain per public hostname and releases the ones
// this App no longer uses. A Domain created by someone else is left alone;
// one owned by another App is a conflict.
func (r *AppReconciler) reconcileDomains(ctx context.Context, app *kwerftv1.App, rd *appRender) error {
	desired := rd.domains()
	for name, domain := range desired {
		var existing kwerftv1.Domain
		err := r.Get(ctx, client.ObjectKey{Namespace: app.Namespace, Name: name}, &existing)
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return err
		case metav1.IsControlledBy(&existing, app):
		case metav1.GetControllerOf(&existing) != nil:
			return terminalf("HostnameInUse", "%s is already used by %s %q",
				name, metav1.GetControllerOf(&existing).Kind, metav1.GetControllerOf(&existing).Name)
		default:
			continue // a hand-made Domain: use it, don't take it over
		}
		if err := apply(ctx, r.Client, domain); err != nil {
			return fmt.Errorf("apply domain: %w", err)
		}
	}
	var existing kwerftv1.DomainList
	if err := r.List(ctx, &existing, client.InNamespace(app.Namespace), client.MatchingLabels{LabelApp: app.Name}); err != nil {
		return err
	}
	for i := range existing.Items {
		d := &existing.Items[i]
		if _, keep := desired[d.Name]; !keep && metav1.IsControlledBy(d, app) {
			if err := client.IgnoreNotFound(r.Delete(ctx, d)); err != nil {
				return err
			}
		}
	}
	return nil
}

// reconcileRoutes applies the desired HTTPRoutes and removes ones for ports
// that are no longer public.
func (r *AppReconciler) reconcileRoutes(ctx context.Context, app *kwerftv1.App, rd *appRender) error {
	held, err := wonHostnames(ctx, r.Client, app.Namespace)
	if err != nil {
		return err
	}
	desired := rd.routes(held)
	for _, route := range desired {
		if err := apply(ctx, r.Client, route); err != nil {
			if meta.IsNoMatchError(err) {
				return terminalf("GatewayAPIMissing", "Gateway API is not installed in this cluster; re-run the installer")
			}
			return fmt.Errorf("apply route: %w", err)
		}
	}
	var existing gwv1.HTTPRouteList
	if err := r.List(ctx, &existing, client.InNamespace(app.Namespace), client.MatchingLabels{LabelApp: app.Name}); err != nil {
		if meta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	for i := range existing.Items {
		route := &existing.Items[i]
		if _, keep := desired[route.Name]; !keep && metav1.IsControlledBy(route, app) {
			if err := client.IgnoreNotFound(r.Delete(ctx, route)); err != nil {
				return err
			}
		}
	}
	return nil
}

// wonHostnames maps each hostname the project (namespace) holds to the
// Gateway listener serving it. A project holds a hostname when one of its
// Domains for it won the claim: the Domain reconciler set status.listener
// for the Domain's current generation (only it writes Domain status; no
// developer role may) and the hostname cannot change after creation.
//
// This is the isolation guarantee for the shared wildcard listener, which
// admits routes from every project namespace: only the App reconciler
// writes HTTPRoutes in project namespaces (no console role may), and it
// attaches one to a listener only for a hostname held here.
func wonHostnames(ctx context.Context, c client.Reader, namespace string) (map[string]string, error) {
	var domains kwerftv1.DomainList
	if err := c.List(ctx, &domains, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range domains.Items {
		if d.Status.Listener == "" || d.Status.ObservedGeneration != d.Generation || !d.DeletionTimestamp.IsZero() {
			continue
		}
		if d.Status.Listener != WildcardListener && d.Status.Listener != ListenerName(d.Spec.Hostname) {
			continue // not a listener this hostname can have
		}
		out[d.Spec.Hostname] = d.Status.Listener
	}
	return out, nil
}

// appsForDomain enqueues the Apps a Domain's state matters to: its owner and
// every App in the project with a port on its hostname (a hand-made Domain).
func (r *AppReconciler) appsForDomain(ctx context.Context, obj client.Object) []reconcile.Request {
	d, ok := obj.(*kwerftv1.Domain)
	if !ok {
		return nil
	}
	var apps kwerftv1.AppList
	if err := r.List(ctx, &apps, client.InNamespace(d.Namespace)); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, a := range apps.Items {
		owns := metav1.IsControlledBy(d, &a)
		uses := slices.ContainsFunc(a.Spec.Ports, func(p kwerftv1.AppPort) bool { return p.Public == d.Spec.Hostname })
		if owns || uses {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&a)})
		}
	}
	return reqs
}

// resolvedImage is what an App (or a Task from it) runs, and for Git apps
// the Build and commit it came from.
type resolvedImage struct {
	image, build, commit string
}

// resolveImage returns the image to run, or nil while a Git app has none
// yet. Image apps run their reference. Git apps run spec.source.git.pinnedImage
// when a rollback set it, else the image of their newest Succeeded Build with
// spec.deploy (by completion time), pinned to its digest. Once all builds
// are gone, the image already running stays.
func resolveImage(ctx context.Context, c client.Reader, app *kwerftv1.App) (*resolvedImage, error) {
	if src := app.Spec.Source.Image; src != nil {
		return &resolvedImage{image: src.Ref}, nil
	}
	git := app.Spec.Source.Git
	if git == nil {
		return nil, nil
	}
	var list kwerftv1.BuildList
	if err := c.List(ctx, &list, client.InNamespace(app.Namespace)); err != nil {
		return nil, err
	}
	var newest *kwerftv1.Build
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.App != app.Name || b.Status.Phase != kwerftv1.BuildSucceeded || b.Status.Image == "" {
			continue
		}
		if git.PinnedImage != "" {
			if deployImage(b) == git.PinnedImage || b.Status.Image == git.PinnedImage {
				return &resolvedImage{image: git.PinnedImage, build: b.Name, commit: b.Spec.Commit}, nil
			}
			continue
		}
		if b.Spec.Deploy && newerBuild(b, newest) {
			newest = b
		}
	}
	switch {
	case git.PinnedImage != "":
		return &resolvedImage{image: git.PinnedImage}, nil // its build was pruned
	case newest != nil:
		return &resolvedImage{image: deployImage(newest), build: newest.Name, commit: newest.Spec.Commit}, nil
	case app.Status.Image != "":
		r := &resolvedImage{image: app.Status.Image}
		if h := app.Status.History; len(h) > 0 && h[0].Image == app.Status.Image {
			r.build, r.commit = h[0].Build, h[0].Commit
		}
		return r, nil
	}
	return nil, nil
}

// newerBuild reports whether b completed after cur (nil: any b is newer).
func newerBuild(b, cur *kwerftv1.Build) bool {
	if cur == nil {
		return true
	}
	bt, ct := b.Status.CompletionTime, cur.Status.CompletionTime
	switch {
	case bt == nil && ct == nil, bt != nil && ct != nil && bt.Equal(ct):
		return b.Status.Number > cur.Status.Number
	case bt == nil:
		return false
	case ct == nil:
		return true
	}
	return ct.Before(bt)
}

// appsForBuild enqueues a successful Build's App: it may be the image to run.
func appsForBuild(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*kwerftv1.Build)
	if !ok || b.Status.Phase != kwerftv1.BuildSucceeded {
		return nil
	}
	return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: b.Namespace, Name: b.Spec.App}}}
}

// appsMountingVolume enqueues the Apps that mount a Volume, so an App waiting
// for it starts once it exists.
func (r *AppReconciler) appsMountingVolume(ctx context.Context, vol client.Object) []reconcile.Request {
	var apps kwerftv1.AppList
	if err := r.List(ctx, &apps, client.InNamespace(vol.GetNamespace())); err != nil {
		return nil
	}
	var reqs []reconcile.Request
	for _, a := range apps.Items {
		if slices.ContainsFunc(a.Spec.Volumes, func(v kwerftv1.AppVolume) bool { return v.Volume == vol.GetName() }) {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&a)})
		}
	}
	return reqs
}

func (r *AppReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Status writes do not bump the generation, so they do not re-trigger.
		// Annotations do not either, but a restart request is one.
		For(&kwerftv1.App{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&kwerftv1.Volume{}, handler.EnqueueRequestsFromMapFunc(r.appsMountingVolume)).
		// A successful build is a new image to roll out (Git apps).
		Watches(&kwerftv1.Build{}, handler.EnqueueRequestsFromMapFunc(appsForBuild)).
		Owns(&appsv1.Deployment{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Owns(&gwv1.HTTPRoute{}).
		// Owned or hand-made: a Domain's claim decides whether routes attach.
		Watches(&kwerftv1.Domain{}, handler.EnqueueRequestsFromMapFunc(r.appsForDomain)).
		Named("app").
		Complete(r)
}
