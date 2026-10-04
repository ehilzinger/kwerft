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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

const (
	httpListener    = "http"
	consoleListener = "console-https"
	consoleSecret   = "kwerft-console-tls"
	// Gateway API allows 64 listeners; two are http and the console.
	maxDomainListeners = 62
)

var certificateGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Certificate"}

// gatewayRequest is the single work item: every change recomputes the whole
// Gateway, because listeners are shared state across all Domains.
var gatewayRequest = reconcile.Request{NamespacedName: types.NamespacedName{Namespace: GatewayNamespace, Name: GatewayName}}

// DomainReconciler owns the shared Gateway. It builds the listener list from
// scratch — plain HTTP, the console, and one HTTPS listener per Domain — and
// reports each Domain's certificate state, which cert-manager provides via
// the Gateway's cluster-issuer annotation.
type DomainReconciler struct {
	client.Client

	ConsoleDomain string // console hostname; empty skips the console listener
	GatewayClass  string // e.g. traefik
	ClusterIssuer string // cert-manager ClusterIssuer; empty disables certificates
}

func (r *DomainReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
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
	if r.ConsoleDomain != "" {
		listeners = append(listeners, httpsListener(consoleListener, r.ConsoleDomain, consoleSecret,
			gwv1ac.RouteNamespaces().WithFrom(gwv1.NamespacesFromSame)))
	}

	// Decide every Domain's fate before touching the Gateway.
	verdicts := map[types.UID]*readiness{}
	claimed := map[string]bool{}
	served := 0
	for _, d := range domains {
		host := d.Spec.Hostname
		switch {
		case host == r.ConsoleDomain:
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "ReservedHostname", host + " is the console's hostname"}
		case claimed[host]:
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "HostnameConflict", host + " is already claimed by an older Domain in another project"}
		case served >= maxDomainListeners:
			verdicts[d.UID] = &readiness{metav1.ConditionFalse, "ListenerLimitReached",
				fmt.Sprintf("the Gateway serves at most %d domains; use a wildcard domain for more", maxDomainListeners)}
		default:
			claimed[host] = true
			served++
			name := ListenerName(host)
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
		gw.WithAnnotations(map[string]string{"cert-manager.io/cluster-issuer": r.ClusterIssuer})
	}
	if err := apply(ctx, r.Client, gw); err != nil {
		return ctrl.Result{}, fmt.Errorf("apply gateway: %w", err)
	}

	// Report per-Domain state; certificates are polled while issuing.
	requeue := false
	for _, d := range domains {
		orig := d.DeepCopy()
		verdict, ok := verdicts[d.UID]
		if ok {
			d.Status.Listener = ""
			d.Status.NotAfter = nil
		} else {
			d.Status.Listener = ListenerName(d.Spec.Hostname)
			verdict = r.certificateState(ctx, d)
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
	if requeue {
		// Certificate events also trigger us when cert-manager is installed;
		// this covers clusters where it is not (yet).
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// certificateState reads the cert-manager Certificate that the gateway-shim
// creates for the Domain's listener (named after its secret).
func (r *DomainReconciler) certificateState(ctx context.Context, d *kwerftv1.Domain) *readiness {
	if r.ClusterIssuer == "" {
		return &readiness{metav1.ConditionFalse, "CertificatesDisabled", "No certificate issuer is configured; HTTPS uses the ingress default certificate"}
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	key := client.ObjectKey{Namespace: GatewayNamespace, Name: ListenerName(d.Spec.Hostname) + "-tls"}
	if err := r.Get(ctx, key, cert); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return &readiness{metav1.ConditionFalse, "CertificatePending", "Waiting for cert-manager to request a certificate"}
		}
		return &readiness{metav1.ConditionUnknown, "CertificateUnknown", err.Error()}
	}

	if s, found, _ := unstructured.NestedString(cert.Object, "status", "notAfter"); found {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			d.Status.NotAfter = &metav1.Time{Time: t}
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
			if d.Status.NotAfter != nil {
				msg = "Certificate valid until " + d.Status.NotAfter.UTC().Format("2 Jan 2006")
			}
			return &readiness{metav1.ConditionTrue, "CertificateIssued", msg}
		}
		return &readiness{metav1.ConditionFalse, "CertificateIssuing", msg}
	}
	return &readiness{metav1.ConditionFalse, "CertificateIssuing", "Certificate requested"}
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
		WatchesRawSource(source.Channel(kick, toGateway))

	// Watch certificates only where cert-manager is installed.
	if _, err := mgr.GetRESTMapper().RESTMapping(certificateGVK.GroupKind(), certificateGVK.Version); err == nil {
		cert := &unstructured.Unstructured{}
		cert.SetGroupVersionKind(certificateGVK)
		b = b.Watches(cert, toGateway)
	}
	return b.Complete(r)
}
