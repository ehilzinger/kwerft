package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	metav1ac "k8s.io/client-go/applyconfigurations/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Console settings (api/v1alpha1 ConsoleSettings) as the Domain reconciler
// applies them: the console's own listeners and routes, which move to a new
// hostname only once its certificate is issued, and the optional wildcard
// listener for the apps base domain with a DNS-01 certificate.

const (
	// ConsoleService is the console's Service (Helm chart) behind its routes.
	ConsoleService     = "kwerft"
	consoleServicePort = 80

	// Routes for the console, in the Gateway's namespace. The chart used to
	// render "kwerft-console" and "kwerft-console-redirect"; these names
	// differ so a Helm upgrade that prunes those cannot delete these.
	consoleRoute         = "kwerft-console-https"
	consoleRedirectRoute = "kwerft-console-http"
	consolePreviousRoute = "kwerft-console-previous"

	// WildcardListener serves every app hostname one label below the apps
	// domain when tls is dns01; its certificate is a cert-manager
	// Certificate of Kwerft's own (not the gateway-shim's), named after its
	// secret so the shim also sees it is not its own to manage.
	WildcardListener = "apps-wildcard"
	WildcardSecret   = "kwerft-apps-wildcard-tls"
	// DNSIssuer is the namespaced ACME Issuer with the DNS-01 solver. Being
	// namespaced, it reads the token from kwerft-system, not cert-manager's
	// namespace, and only the wildcard Certificate uses it.
	DNSIssuer = "kwerft-dns01"
	// DNSTokenSecret holds the DNS provider's API token (key "token").
	// Kwerft creates it empty; owners and admins may patch it, nobody reads it.
	DNSTokenSecret = "kwerft-dns-token"
	DNSTokenKey    = "token"

	// OIDCSecret holds the single sign-on client secret (key
	// "clientSecret", ConsoleSettings.spec.sso). Like the DNS token, Kwerft
	// creates it empty, owners and admins may patch it, and only the
	// console's own identity reads it.
	OIDCSecret    = "kwerft-oidc-client"
	OIDCSecretKey = "clientSecret"
	// AnnotationOIDCSecretUpdated records on ConsoleSettings when the client
	// secret was last written, so Settings can tell whether one is set.
	AnnotationOIDCSecretUpdated = "kwerft.dev/oidc-secret-updated-at"

	// AnnotationDNSTokenUpdated is set on ConsoleSettings when the console
	// writes a new token, so a failed wildcard certificate is retried at once
	// instead of after cert-manager's backoff.
	AnnotationDNSTokenUpdated = "kwerft.dev/dns-token-updated-at"
	annotationRetriedFor      = "kwerft.dev/retried-for-token"

	// Hetzner's cert-manager webhook (installed by install.sh).
	hetznerGroupName  = "acme.hetzner.com"
	hetznerSolverName = "hetzner"

	// previousGrace is how long the old console hostname keeps redirecting.
	previousGrace = 24 * time.Hour
)

var (
	issuerGVK        = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "Issuer"}
	clusterIssuerGVK = schema.GroupVersionKind{Group: "cert-manager.io", Version: "v1", Kind: "ClusterIssuer"}
)

// consolePlan is where the console is served during one reconcile.
type consolePlan struct {
	active   string // serving now; what browsers should use
	pending  string // requested, waiting for its certificate
	previous string // redirects to active for previousGrace after a switch
	switched *metav1.Time
}

// hosts are the hostnames the console's routes answer for (not previous,
// which only redirects).
func (p consolePlan) hosts() []string {
	var out []string
	for _, h := range []string{p.active, p.pending} {
		if h != "" {
			out = append(out, h)
		}
	}
	return out
}

func (p consolePlan) reserved(host string) bool {
	return host != "" && (host == p.active || host == p.pending || host == p.previous)
}

// ConsoleListenerName is the Gateway listener for a console hostname. Named
// after the hostname, like Domain listeners, so its certificate (secret
// "<listener>-tls") stays with the name when the console moves.
func ConsoleListenerName(host string) string { return "c" + ListenerName(host)[1:] }

func consoleSecretName(host string) string { return ConsoleListenerName(host) + "-tls" }

// loadSettings returns the singleton, or nil when it (or its CRD) is missing.
func (r *DomainReconciler) loadSettings(ctx context.Context) (*kwerftv1.ConsoleSettings, error) {
	var s kwerftv1.ConsoleSettings
	err := r.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s)
	switch {
	case err == nil:
		return &s, nil
	case apierrors.IsNotFound(err) || meta.IsNoMatchError(err):
		return nil, nil
	default:
		return nil, err
	}
}

// planConsole decides which console hostnames to serve. The spec wins over
// the --console-domain flag; a new hostname is served next to the old one
// until its certificate is ready, then becomes the active one.
func (r *DomainReconciler) planConsole(ctx context.Context, s *kwerftv1.ConsoleSettings, now time.Time) consolePlan {
	desired := r.ConsoleDomain
	var st kwerftv1.ConsoleSettingsStatus
	if s != nil {
		if s.Spec.ConsoleDomain != "" {
			desired = s.Spec.ConsoleDomain
		}
		st = s.Status
	}
	if desired == "" {
		return consolePlan{}
	}
	// Without a recorded active hostname the console has been served on the
	// flag's (the installer's) name, so a new spec still waits for its
	// certificate before taking over.
	p := consolePlan{active: st.ConsoleDomain, previous: st.PreviousConsoleDomain, switched: st.SwitchedAt}
	if p.active == "" {
		p.active = r.ConsoleDomain
	}
	if p.active == "" {
		p.active = desired
	}
	if desired != p.active {
		p.pending = desired
	}
	// Promote once the new name has a certificate (or none is ever issued).
	if p.pending != "" && (r.ClusterIssuer == "" || r.certificateReady(ctx, consoleSecretName(p.pending))) {
		p.previous, p.active, p.pending = p.active, p.pending, ""
		p.switched = &metav1.Time{Time: now}
	}
	if p.previous == p.active || p.previous == p.pending ||
		p.switched == nil || now.Sub(p.switched.Time) >= previousGrace {
		p.previous = ""
	}
	return p
}

// consoleListeners are the console's HTTPS listeners, open only to the
// console's own namespace.
func consoleListeners(p consolePlan) []*gwv1ac.ListenerApplyConfiguration {
	var out []*gwv1ac.ListenerApplyConfiguration
	for _, h := range []string{p.active, p.pending, p.previous} {
		if h != "" {
			out = append(out, httpsListener(ConsoleListenerName(h), h, consoleSecretName(h),
				gwv1ac.RouteNamespaces().WithFrom(gwv1.NamespacesFromSame)))
		}
	}
	return out
}

// reconcileConsoleRoutes renders the console's HTTPRoutes: HTTPS to the
// console Service, HTTP redirecting to HTTPS, and the previous hostname
// redirecting to the active one.
func (r *DomainReconciler) reconcileConsoleRoutes(ctx context.Context, p consolePlan) error {
	labels := map[string]string{LabelManagedBy: ManagedByKwerft}
	route := func(name string) *gwv1ac.HTTPRouteApplyConfiguration {
		return gwv1ac.HTTPRoute(name, GatewayNamespace).WithLabels(labels)
	}
	parent := func(section string) *gwv1ac.ParentReferenceApplyConfiguration {
		return gwv1ac.ParentReference().WithName(GatewayName).WithSectionName(gwv1.SectionName(section))
	}
	hostnames := func(hs ...string) []gwv1.Hostname {
		out := make([]gwv1.Hostname, 0, len(hs))
		for _, h := range hs {
			out = append(out, gwv1.Hostname(h))
		}
		return out
	}

	hosts := p.hosts()
	if len(hosts) == 0 {
		for _, n := range []string{consoleRoute, consoleRedirectRoute, consolePreviousRoute} {
			if err := r.deleteManagedRoute(ctx, n); err != nil {
				return err
			}
		}
		return nil
	}
	https := gwv1ac.HTTPRouteSpec().WithHostnames(hostnames(hosts...)...).
		WithRules(gwv1ac.HTTPRouteRule().WithBackendRefs(gwv1ac.HTTPBackendRef().
			WithName(ConsoleService).WithPort(consoleServicePort)))
	for _, h := range hosts {
		https.WithParentRefs(parent(ConsoleListenerName(h)))
	}
	if err := apply(ctx, r.Client, route(consoleRoute).WithSpec(https)); err != nil {
		return fmt.Errorf("apply console route: %w", err)
	}
	// Plain HTTP goes to HTTPS on the same name, the previous one included.
	// ACME HTTP-01 challenges use cert-manager's own, more specific routes.
	plain := hosts
	if p.previous != "" {
		plain = append(plain, p.previous)
	}
	redirect := gwv1ac.HTTPRouteSpec().WithParentRefs(parent(httpListener)).WithHostnames(hostnames(plain...)...).
		WithRules(gwv1ac.HTTPRouteRule().WithFilters(gwv1ac.HTTPRouteFilter().
			WithType(gwv1.HTTPRouteFilterRequestRedirect).
			WithRequestRedirect(gwv1ac.HTTPRequestRedirectFilter().WithScheme("https").WithStatusCode(301))))
	if err := apply(ctx, r.Client, route(consoleRedirectRoute).WithSpec(redirect)); err != nil {
		return fmt.Errorf("apply console redirect route: %w", err)
	}

	if p.previous == "" {
		return r.deleteManagedRoute(ctx, consolePreviousRoute)
	}
	// Pages redirect (302, not 301: a move can be undone, and browsers cache
	// 301s for good). The API keeps answering on the old name, so pages open
	// there learn of the move by polling instead of failing on a cross-origin
	// redirect, and can send the browser over.
	moved := gwv1ac.HTTPRouteSpec().
		WithParentRefs(parent(ConsoleListenerName(p.previous))).
		WithHostnames(gwv1.Hostname(p.previous)).
		WithRules(
			gwv1ac.HTTPRouteRule().
				WithMatches(gwv1ac.HTTPRouteMatch().WithPath(gwv1ac.HTTPPathMatch().
					WithType(gwv1.PathMatchPathPrefix).WithValue("/api/"))).
				WithBackendRefs(gwv1ac.HTTPBackendRef().WithName(ConsoleService).WithPort(consoleServicePort)),
			gwv1ac.HTTPRouteRule().WithFilters(gwv1ac.HTTPRouteFilter().
				WithType(gwv1.HTTPRouteFilterRequestRedirect).
				WithRequestRedirect(gwv1ac.HTTPRequestRedirectFilter().
					WithScheme("https").WithHostname(gwv1.PreciseHostname(p.active)).WithStatusCode(302))))
	if err := apply(ctx, r.Client, route(consolePreviousRoute).WithSpec(moved)); err != nil {
		return fmt.Errorf("apply previous console route: %w", err)
	}
	return nil
}

func (r *DomainReconciler) deleteManagedRoute(ctx context.Context, name string) error {
	var route gwv1.HTTPRoute
	err := r.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, &route)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if route.Labels[LabelManagedBy] != ManagedByKwerft {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, &route))
}

// wildcardPlan is the apps wildcard for one reconcile.
type wildcardPlan struct {
	domain  string // apps base domain; empty when no wildcard is wanted
	serving bool   // certificate issued: the listener exists and Domains use it
	problem *readiness
	cert    *kwerftv1.CertificateState
}

// covers reports whether host is served by the wildcard listener: exactly one
// label below the apps domain (a wildcard certificate covers no more).
func (w wildcardPlan) covers(host string) bool {
	if !w.serving {
		return false
	}
	label, ok := strings.CutSuffix(host, "."+w.domain)
	return ok && label != "" && !strings.Contains(label, ".")
}

// reconcileWildcard ensures the DNS-01 Issuer and the wildcard Certificate
// exist when the settings ask for them, and removes them otherwise.
func (r *DomainReconciler) reconcileWildcard(ctx context.Context, s *kwerftv1.ConsoleSettings) (wildcardPlan, error) {
	var w wildcardPlan
	if s != nil {
		// Before DNS-01 is on: the console stores the token first.
		if err := r.ensureTokenSecret(ctx); err != nil {
			return w, err
		}
	}
	if s == nil || s.Spec.AppsDomain == "" || s.Spec.TLS != kwerftv1.TLSDNS01 || s.Spec.DNS == nil {
		return w, r.removeWildcard(ctx)
	}
	w.domain = s.Spec.AppsDomain
	if r.ClusterIssuer == "" {
		w.problem = &readiness{metav1.ConditionFalse, "CertificatesDisabled", "No certificate issuer is configured, so there is no wildcard certificate"}
		return w, nil
	}

	// The ACME account settings come from the installer's ClusterIssuer.
	ci := &unstructured.Unstructured{}
	ci.SetGroupVersionKind(clusterIssuerGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: r.ClusterIssuer}, ci); err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			w.problem = &readiness{metav1.ConditionFalse, "IssuerMissing", "ClusterIssuer " + r.ClusterIssuer + " not found; re-run the installer"}
			return w, nil
		}
		return w, err
	}
	server, _, _ := unstructured.NestedString(ci.Object, "spec", "acme", "server")
	email, _, _ := unstructured.NestedString(ci.Object, "spec", "acme", "email")
	if server == "" {
		w.problem = &readiness{metav1.ConditionFalse, "IssuerMissing", "ClusterIssuer " + r.ClusterIssuer + " is not an ACME issuer"}
		return w, nil
	}

	owner := []any{map[string]any{
		"apiVersion": kwerftv1.GroupVersion.String(), "kind": "ConsoleSettings",
		"name": s.Name, "uid": string(s.UID), "controller": true, "blockOwnerDeletion": true,
	}}
	acme := map[string]any{
		"server":              server,
		"privateKeySecretRef": map[string]any{"name": DNSIssuer + "-account"},
		"solvers": []any{map[string]any{"dns01": map[string]any{"webhook": map[string]any{
			"groupName":  hetznerGroupName,
			"solverName": hetznerSolverName,
			"config":     map[string]any{"tokenSecretKeyRef": map[string]any{"name": DNSTokenSecret, "key": DNSTokenKey}},
		}}}},
	}
	if email != "" {
		acme["email"] = email
	}
	issuer := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": DNSIssuer, "namespace": GatewayNamespace,
			"labels": map[string]any{LabelManagedBy: ManagedByKwerft}, "ownerReferences": owner},
		"spec": map[string]any{"acme": acme},
	}}
	issuer.SetGroupVersionKind(issuerGVK)
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(issuer), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		if meta.IsNoMatchError(err) {
			w.problem = &readiness{metav1.ConditionFalse, "CertManagerMissing", "cert-manager is not installed; re-run the installer"}
			return w, nil
		}
		return w, fmt.Errorf("apply issuer: %w", err)
	}

	host := "*." + w.domain
	cert := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": WildcardSecret, "namespace": GatewayNamespace,
			"labels": map[string]any{LabelManagedBy: ManagedByKwerft}, "ownerReferences": owner},
		"spec": map[string]any{
			"secretName": WildcardSecret,
			"dnsNames":   []any{host},
			"issuerRef":  map[string]any{"name": DNSIssuer, "kind": "Issuer", "group": "cert-manager.io"},
		},
	}}
	cert.SetGroupVersionKind(certificateGVK)
	if err := r.Apply(ctx, client.ApplyConfigurationFromUnstructured(cert), client.FieldOwner(FieldOwner), client.ForceOwnership); err != nil {
		return w, fmt.Errorf("apply wildcard certificate: %w", err)
	}

	state, notAfter := r.certState(ctx, WildcardSecret)
	w.cert = certificateState(WildcardSecret, "apps-wildcard", []string{host}, state, notAfter)
	// Once issued, keep serving through renewals: a valid certificate stays
	// in the secret even while a renewal is failing.
	w.serving = state.status == metav1.ConditionTrue || (notAfter != nil && notAfter.After(time.Now()))
	if state.status != metav1.ConditionTrue {
		r.retryAfterNewToken(ctx, s)
	}
	return w, nil
}

// retryAfterNewToken asks cert-manager to try the wildcard again right away
// when the token changed since the last attempt (as `cmctl renew` does).
func (r *DomainReconciler) retryAfterNewToken(ctx context.Context, s *kwerftv1.ConsoleSettings) {
	token := s.Annotations[AnnotationDNSTokenUpdated]
	if token == "" {
		return
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	if err := r.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: WildcardSecret}, cert); err != nil {
		return
	}
	if cert.GetAnnotations()[annotationRetriedFor] == token {
		return
	}
	orig := cert.DeepCopy()
	conds, _, _ := unstructured.NestedSlice(cert.Object, "status", "conditions")
	kept := []any{}
	for _, c := range conds {
		if m, _ := c.(map[string]any); m["type"] != "Issuing" {
			kept = append(kept, c)
		}
	}
	kept = append(kept, map[string]any{
		"type": "Issuing", "status": "True", "reason": "ManuallyTriggered",
		"message": "Retrying with the new DNS API token", "lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
	})
	_ = unstructured.SetNestedSlice(cert.Object, kept, "status", "conditions")
	if err := r.Status().Patch(ctx, cert, client.MergeFrom(orig)); err != nil {
		return // best effort: cert-manager retries on its own schedule anyway
	}
	patch := client.MergeFrom(cert.DeepCopy())
	ann := cert.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	ann[annotationRetriedFor] = token
	cert.SetAnnotations(ann)
	_ = r.Patch(ctx, cert, patch)
}

// ensureTokenSecret creates the empty write-only Secrets of the settings —
// the DNS token and the single sign-on client secret — so owners and admins
// can set them with "patch" alone: their role cannot create Secrets, and no
// role can read these.
func (r *DomainReconciler) ensureTokenSecret(ctx context.Context) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	for _, name := range []string{DNSTokenSecret, OIDCSecret} {
		var sec corev1.Secret
		err := reader.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, &sec)
		if !apierrors.IsNotFound(err) {
			if err != nil {
				return err
			}
			continue
		}
		sec = corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: GatewayNamespace,
				Labels: map[string]string{LabelManagedBy: ManagedByKwerft}},
			Type: corev1.SecretTypeOpaque,
		}
		if err := r.Create(ctx, &sec); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	return nil
}

// removeWildcard deletes the wildcard Certificate and Issuer (only Kwerft's
// own). The token Secret and the issued certificate's Secret stay, so turning
// DNS-01 back on needs neither a new token nor, within its lifetime, a new
// certificate.
func (r *DomainReconciler) removeWildcard(ctx context.Context) error {
	for _, obj := range []struct {
		gvk  schema.GroupVersionKind
		name string
	}{{certificateGVK, WildcardSecret}, {issuerGVK, DNSIssuer}} {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(obj.gvk)
		err := r.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: obj.name}, u)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return err
		}
		if u.GetLabels()[LabelManagedBy] != ManagedByKwerft {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, u)); err != nil {
			return err
		}
	}
	return nil
}

// wildcardListener admits routes from project namespaces only. Which project
// may use which hostname is decided by Domain claims: the App reconciler
// attaches a route here only for a hostname its project's Domain won (see
// docs in app_controller.go), and no console role can write HTTPRoutes.
func wildcardListener(domain string) *gwv1ac.ListenerApplyConfiguration {
	return httpsListener(WildcardListener, "*."+domain, WildcardSecret,
		gwv1ac.RouteNamespaces().
			WithFrom(gwv1.NamespacesFromSelector).
			WithSelector(metav1ac.LabelSelector().WithMatchExpressions(
				metav1ac.LabelSelectorRequirement().WithKey(LabelProject).WithOperator(metav1.LabelSelectorOpExists))))
}

// certificateReady reports whether the Certificate named name is Ready.
func (r *DomainReconciler) certificateReady(ctx context.Context, name string) bool {
	state, _ := r.certState(ctx, name)
	return state.status == metav1.ConditionTrue
}

func certificateState(name, purpose string, hosts []string, state *readiness, notAfter *metav1.Time) *kwerftv1.CertificateState {
	return &kwerftv1.CertificateState{
		Name: name, Purpose: purpose, Hostnames: hosts,
		Ready: state.status == metav1.ConditionTrue, Reason: state.reason, Message: state.message, NotAfter: notAfter,
	}
}

// publicAddresses are the nodes' external IPs (k3s: --node-external-ip),
// falling back to internal ones on clusters without any.
func (r *DomainReconciler) publicAddresses(ctx context.Context) []string {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil
	}
	var external, internal []string
	for _, n := range nodes.Items {
		for _, a := range n.Status.Addresses {
			switch a.Type {
			case corev1.NodeExternalIP:
				external = append(external, a.Address)
			case corev1.NodeInternalIP:
				internal = append(internal, a.Address)
			}
		}
	}
	if len(external) == 0 {
		external = internal
	}
	slices.Sort(external)
	return slices.Compact(external)
}
