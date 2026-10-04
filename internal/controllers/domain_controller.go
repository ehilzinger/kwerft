package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	httpListener = "http"
	// Gateway API allows 64 listeners: http, the console (up to three while
	// it moves to a new hostname) and the apps wildcard leave 59 for Domains.
	maxDomainListeners = 59
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// gatewayRequest is the single work item: every change recomputes the whole
// Gateway, because listeners are shared state across all Domains.
var gatewayRequest = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: GatewayNamespace, Name: GatewayName}}

// DomainReconciler owns the shared Gateway. It builds the listener list from
// scratch — plain HTTP, the console, the apps wildcard and one HTTPS listener
// per Domain — and reports each Domain's certificate state, which
// cert-manager provides via the Gateway's cluster-issuer annotation. The
// ConsoleSettings singleton, when present, overrides the console hostname and
// adds the wildcard; the reconciler records what it did in its status.
type DomainReconciler struct {
	client.Client
	// APIReader reads the DNS token Secret without caching every Secret in
	// the cluster; nil falls back to the client (tests).
	APIReader client.Reader

	ConsoleDomain string // console hostname from the flag; ConsoleSettings.spec wins
	GatewayClass  string // e.g. traefik
	ClusterIssuer string // cert-manager ClusterIssuer; empty disables certificates

	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

func (r *DomainReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *DomainReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	settings, err := r.loadSettings(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	console := r.planConsole(ctx, settings, r.now())
	wildcard, err := r.reconcileWildcard(ctx, settings)
	if err != nil {
		return ctrl.Result{}, err
	}

	var list kwerftv1.DomainList
	if err := r.List(ctx, &list); err != nil {
		return ctrl.Result{}, err
	}
	domains := make([]*kwerftv1.Domain, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() {
			domains = append(domains, &list.Items[i])
		}
	}
	// Oldest claim wins a hostname; ties broken by namespace/name for stability.
	sort.Slice(domains, func(i, j int) bool {
		a, b := domains[i], domains[j]
		if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
			return a.CreationTimestamp.Before(&b.CreationTimestamp)
		}
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})

	listeners := []*gwv1ac.ListenerApplyConfiguration{
		gwv1ac.Listener().
			WithName(httpListener).
			WithProtocol(gwv1.HTTPProtocolType).
			WithPort(80).
			// TODO(phase-4): restrict which projects may attach plain-HTTP routes.
			WithAllowedRoutes(gwv1ac.AllowedRoutes().WithNamespaces(gwv1ac.RouteNamespaces().WithFrom(gwv1.NamespacesFromAll))),
	}
	listeners = append(listeners, consoleListeners(console)...)
	if wildcard.serving {
		listeners = append(listeners, wildcardListener(wildcard.domain))
	}

	// Decide every Domain's fate before touching the Gateway. A Domain gets
	// either a verdict (it serves nothing) or a listener.
	verdicts := map[types.UID]*readiness{}
	assigned := map[types.UID]string{}
	claimed := map[string]bool{}
	served := 0
	for _, d := range domains {
		host := d.Spec.Hostname
		switch {
		case console.reserved(host):
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "ReservedHostname", host + " is the console's hostname"}
		case claimed[host]:
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "HostnameConflict", host + " is already claimed by an older Domain in another project"}
		case wildcard.covers(host):
			// A shared listener: the claim is what keeps other projects off
			// this hostname, because routes attach only for won claims.
			claimed[host] = true
			assigned[d.UID] = WildcardListener
		case served >= maxDomainListeners:
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "ListenerLimitReached",
				fmt.Sprintf("the Gateway serves at most %d hostnames with listeners of their own; use an apps domain with DNS-01 (Settings) for more", maxDomainListeners)}
		default:
			claimed[host] = true
			served++
			name := ListenerName(host)
			assigned[d.UID] = name
			listeners = append(listeners, httpsListener(name, host, name+"-tls",
				gwv1ac.RouteNamespaces().
					WithFrom(gwv1.NamespacesFromSelector).
					// Only routes from the Domain's own project may use the hostname.
					WithSelector(metav1ac.LabelSelector().WithMatchLabels(map[string]string{"kubernetes.io/metadata.name": d.Namespace}))))
		}
	}

	gw := gwv1ac.Gateway(GatewayName, GatewayNamespace).
		WithLabels(map[string]string{LabelManagedBy: ManagedByKwerft}).
		WithSpec(gwv1ac.GatewaySpec().
			WithGatewayClassName(gwv1.ObjectName(r.GatewayClass)).
			WithListeners(listeners...))
	if r.ClusterIssuer != "" {
		ann := map[string]string{"cert-manager.io/cluster-issuer": r.ClusterIssuer}
		if wildcard.domain != "" {
			// The wildcard has a DNS-01 Certificate of its own; keep the
			// gateway-shim from requesting an HTTP-01 one for it.
			ann["cert-manager.io/ignore-tls-listeners"] = WildcardListener
		}
		gw.WithAnnotations(ann)
	}
	if err := apply(ctx, r.Client, gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply gateway: %w", err)
	}
	if err := r.reconcileConsoleRoutes(ctx, console); err != nil {
		return ctrl.Result{}, err
	}
	cleanup, err := r.cleanupListenerSecrets(ctx, referencedSecrets(listeners))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("clean up certificate secrets: %w", err)
	}

	// Report per-Domain state; certificates are polled while issuing.
	requeue := false
	for _, d := range domains {
		orig := d.DeepCopy()
		verdict, ok := verdicts[d.UID]
		d.Status.NotAfter = nil
		if ok {
			d.Status.Listener = ""
		} else {
			d.Status.Listener = assigned[d.UID]
			secret := d.Status.Listener + "-tls"
			if d.Status.Listener == WildcardListener {
				secret = WildcardSecret
			}
			verdict, d.Status.NotAfter = r.certState(ctx, secret)
			requeue = requeue || verdict.status != metav1.ConditionTrue
		}
		setReady(&d.Status.Conditions, d.Generation, verdict.status, verdict.reason, verdict.message)
		d.Status.ObservedGeneration = d.Generation
		if !equality.Semantic.DeepEqual(orig.Status, d.Status) {
			if err := r.Status().Patch(ctx, d, client.MergeFrom(orig)); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
		}
	}

	wait, err := r.reportSettings(ctx, settings, console, wildcard)
	if err != nil {
		return ctrl.Result{}, err
	}
	if requeue || wait > 0 {
		// Certificate events also trigger us when cert-manager is installed;
		// this covers clusters where it is not (yet).
		after := 30 * time.Second
		if wait > 0 && wait < after {
			after = wait
		}
		return ctrl.Result{RequeueAfter: after}, nil
	}
	if cleanup > 0 {
		return ctrl.Result{RequeueAfter: cleanup}, nil
	}
	return ctrl.Result{}, nil
}

// reportSettings writes what the reconciler did into ConsoleSettings.status
// and returns how soon it should look again (0: no need).
func (r *DomainReconciler) reportSettings(ctx context.Context, s *kwerftv1.ConsoleSettings, p consolePlan, w wildcardPlan) (time.Duration, error) {
	var wait time.Duration
	if p.pending != "" || (w.domain != "" && !w.serving) {
		wait = 30 * time.Second
	}
	if p.previous != "" && p.switched != nil {
		left := max(previousGrace-r.now().Sub(p.switched.Time), time.Second)
		if wait == 0 || left < wait {
			wait = left
		}
	}
	if s == nil {
		return wait, nil
	}
	orig := s.DeepCopy()
	st := &s.Status
	st.ConsoleDomain, st.PreviousConsoleDomain, st.SwitchedAt = p.active, p.previous, p.switched
	st.WildcardDomain = ""
	if w.serving {
		st.WildcardDomain = "*." + w.domain
	}
	st.PublicAddresses = r.publicAddresses(ctx)

	st.Certificates = nil
	if r.ClusterIssuer != "" {
		for _, c := range []struct{ host, purpose string }{{p.active, "console"}, {p.pending, "console-next"}, {p.previous, "console-previous"}} {
			if c.host == "" {
				continue
			}
			state, notAfter := r.certState(ctx, consoleSecretName(c.host))
			st.Certificates = append(st.Certificates, *certificateState(consoleSecretName(c.host), c.purpose, []string{c.host}, state, notAfter))
		}
	}
	if w.cert != nil {
		st.Certificates = append(st.Certificates, *w.cert)
	}

	switch {
	case p.pending != "":
		setReady(&st.Conditions, s.Generation, metav1.ConditionFalse, "ConsoleMoving",
			"Waiting for the certificate of "+p.pending+" before the console moves there")
	case w.problem != nil:
		setReady(&st.Conditions, s.Generation, w.problem.status, w.problem.reason, w.problem.message)
	case w.domain != "" && !w.serving:
		msg := "Waiting for the wildcard certificate *." + w.domain
		if w.cert != nil && w.cert.Message != "" {
			msg += ": " + w.cert.Message
		}
		setReady(&st.Conditions, s.Generation, metav1.ConditionFalse, "WildcardIssuing", msg)
	default:
		setReady(&st.Conditions, s.Generation, metav1.ConditionTrue, "Applied", "The console is served on "+p.active)
	}
	st.ObservedGeneration = s.Generation
	if equality.Semantic.DeepEqual(orig.Status, s.Status) {
		return wait, nil
	}
	if err := r.Status().Patch(ctx, s, client.MergeFrom(orig)); err != nil && !apierrors.IsNotFound(err) {
		return 0, err
	}
	return wait, nil
}

// certState reads the cert-manager Certificate named name (the gateway-shim
// names each after its listener's secret) in the Gateway's namespace.
func (r *DomainReconciler) certState(ctx context.Context, name string) (*readiness, *metav1.Time) {
	if r.ClusterIssuer == "" {
		return &readiness{metav1.ConditionFalse, "CertificatesDisabled", "No certificate issuer is configured; HTTPS uses the ingress default certificate"}, nil
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, cert); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return &readiness{metav1.ConditionFalse, "CertificatePending", "Waiting for cert-manager to request a certificate"}, nil
		}
		return &readiness{metav1.ConditionUnknown, "CertificateUnknown", err.Error()}, nil
	}

	var notAfter *metav1.Time
	if s, found, _ := unstructured.NestedString(cert.Object, "status", "notAfter"); found {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			notAfter = &metav1.Time{Time: t}
		}
	}
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] != "Ready" {
			continue
		}
		msg, _ := m["message"].(string)
		if m["status"] == "True" {
			if notAfter != nil {
				msg = "Certificate valid until " + notAfter.UTC().Format("2 Jan 2006")
			}
			return &readiness{metav1.ConditionTrue, "CertificateIssued", msg}, notAfter
		}
		return &readiness{metav1.ConditionFalse, "CertificateIssuing", msg}, notAfter
	}
	return &readiness{metav1.ConditionFalse, "CertificateIssuing", "Certificate requested"}, notAfter
}

func httpsListener(name, host, secret string, ns *gwv1ac.RouteNamespacesApplyConfiguration) *gwv1ac.ListenerApplyConfiguration {
	return gwv1ac.Listener().
		WithName(gwv1.SectionName(name)).
		WithProtocol(gwv1.HTTPSProtocolType).
		WithPort(443).
		WithHostname(gwv1.Hostname(host)).
		WithTLS(gwv1ac.ListenerTLSConfig().
			WithMode(gwv1.TLSModeTerminate).
			WithCertificateRefs(gwv1ac.SecretObjectReference().WithName(gwv1.ObjectName(secret)))).
		WithAllowedRoutes(gwv1ac.AllowedRoutes().WithNamespaces(ns))
}

var nonListenerChars = regexp.MustCompile(`[^a-z0-9-]+`)

// ListenerName is the Gateway listener for a hostname: readable, stable, and
// collision-free thanks to a hash suffix (a-b.c and a.b-c differ).
func ListenerName(host string) string {
	sum := sha256.Sum256([]byte(host))
	readable := strings.Trim(nonListenerChars.ReplaceAllString(strings.ToLower(host), "-"), "-")
	if len(readable) > 40 {
		readable = strings.TrimRight(readable[:40], "-")
	}
	return "d-" + readable + "-" + hex.EncodeToString(sum[:])[:8]
}

func (r *DomainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	toGateway := handler.EnqueueRequestsFromMapFunc(func(context.Context, client.Object) []reconcile.Request {
		return []reconcile.Request{gatewayRequest}
	})

	// One event at start so the Gateway exists even before the first Domain.
	kick := make(chan event.GenericEvent, 1)
	kick <- event.GenericEvent{Object: &gwv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: GatewayName, Namespace: GatewayNamespace}}}

	b := ctrl.NewControllerManagedBy(mgr).
		Named("domain").
		Watches(&kwerftv1.Domain{}, toGateway).
		Watches(&gwv1.Gateway{}, toGateway).
		// Spec and annotation changes (a new DNS token), not its own status.
		Watches(&kwerftv1.ConsoleSettings{}, toGateway, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		// The console's routes, so a deleted one comes back.
		Watches(&gwv1.HTTPRoute{}, toGateway, builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
			return o.GetNamespace() == GatewayNamespace && o.GetLabels()[LabelManagedBy] == ManagedByKwerft
		}))).
		// Node addresses are where DNS must point (status.publicAddresses).
		Watches(&corev1.Node{}, toGateway, builder.WithPredicates(predicate.Funcs{
			UpdateFunc: func(e event.UpdateEvent) bool {
				return !equality.Semantic.DeepEqual(e.ObjectOld.(*corev1.Node).Status.Addresses, e.ObjectNew.(*corev1.Node).Status.Addresses)
			},
		})).
		WatchesRawSource(source.Channel(kick, toGateway))

	// Watch certificates only where cert-manager is installed.
	if _, err := mgr.GetRESTMapper().RESTMapping(certificateGVK.GroupKind(), certificateGVK.Version); err == nil {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		b = b.Watches(cert, toGateway)
	}
	return b.Complete(r)
}
