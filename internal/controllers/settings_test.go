package controllers

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// Console settings: tests share the singleton, so each one removes it again
// and waits until the console is back on the flag's hostname.

func useSettings(t *testing.T, spec kwerftv1.ConsoleSettingsSpec) *kwerftv1.ConsoleSettings {
	t.Helper()
	ctx := context.Background()
	s := &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}, Spec: spec}
	if err := k8s.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k8s.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
			t.Error(err)
		}
		domainClock.Reset()
		eventually(t, func() error {
			gw := getGateway(t)
			for _, l := range gw.Spec.Listeners {
				if l.Hostname != nil && string(l.Name) != ConsoleListenerName(testConsoleDomain) &&
					string(l.Name) == ConsoleListenerName(string(*l.Hostname)) {
					return fmt.Errorf("console listener for %s still there", *l.Hostname)
				}
			}
			if listener(gw, ConsoleListenerName(testConsoleDomain)) == nil {
				return errors.New("console not back on its flag hostname")
			}
			return nil
		})
	})
	return s
}

func waitForSettings(t *testing.T, check func(*kwerftv1.ConsoleSettings) error) *kwerftv1.ConsoleSettings {
	t.Helper()
	var s kwerftv1.ConsoleSettings
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		if s.Status.ObservedGeneration != s.Generation {
			return errors.New("status not observed yet")
		}
		return check(&s)
	})
	return &s
}

// issue plays cert-manager: the Certificate named name becomes Ready.
func issue(t *testing.T, name string, hosts ...string) {
	t.Helper()
	ctx := context.Background()
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, cert)
	if apierrors.IsNotFound(err) {
		cert.SetNamespace(GatewayNamespace)
		cert.SetName(name)
		dns := []any{}
		for _, h := range hosts {
			dns = append(dns, h)
		}
		_ = unstructured.SetNestedSlice(cert.Object, dns, "spec", "dnsNames")
		err = k8s.Create(ctx, cert)
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(cert.Object, time.Now().Add(90*24*time.Hour).UTC().Format(time.RFC3339), "status", "notAfter")
	_ = unstructured.SetNestedSlice(cert.Object, []any{map[string]any{"type": "Ready", "status": "True", "message": "Certificate is up to date"}}, "status", "conditions")
	if err := k8s.Update(ctx, cert); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), cert) })
}

func hostnamesOf(r *gwv1.HTTPRoute) []string {
	var out []string
	for _, h := range r.Spec.Hostnames {
		out = append(out, string(h))
	}
	slices.Sort(out)
	return out
}

func TestSettingsMoveConsoleOnceCertificateIsIssued(t *testing.T) {
	requireEnvtest(t)
	const next = "ops.example.net"
	useSettings(t, kwerftv1.ConsoleSettingsSpec{ConsoleDomain: next})

	// Both names are served while the new one waits for its certificate; the
	// console stays where it is.
	eventually(t, func() error {
		gw := getGateway(t)
		if listener(gw, ConsoleListenerName(next)) == nil || listener(gw, ConsoleListenerName(testConsoleDomain)) == nil {
			return errors.New("want listeners for both console hostnames")
		}
		return nil
	})
	s := waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		if reason, err := readyReason(s.Status.Conditions, s.Generation); err != nil || reason != "ConsoleMoving" {
			return fmt.Errorf("reason %q (%v), want ConsoleMoving", reason, err)
		}
		return nil
	})
	if s.Status.ConsoleDomain != testConsoleDomain {
		t.Errorf("status.consoleDomain = %q before the certificate, want the old %q", s.Status.ConsoleDomain, testConsoleDomain)
	}
	eventually(t, func() error {
		if got := hostnamesOf(getRoute(t, GatewayNamespace, consoleRoute)); !slices.Equal(got, []string{testConsoleDomain, next}) {
			return fmt.Errorf("console route hostnames %v", got)
		}
		return nil
	})
	// A project cannot claim the name the console is moving to.
	projectNamespace(t, "squatter")
	d := createDomain(t, "squatter", "next", next)
	waitForDomain(t, d, "ReservedHostname")

	// Certificate issued: the console moves, and the old name redirects.
	issue(t, consoleSecretName(next), next)
	s = waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		if s.Status.ConsoleDomain != next {
			return fmt.Errorf("status.consoleDomain = %q", s.Status.ConsoleDomain)
		}
		return nil
	})
	if s.Status.PreviousConsoleDomain != testConsoleDomain || s.Status.SwitchedAt == nil {
		t.Errorf("previous = %q at %v, want %q", s.Status.PreviousConsoleDomain, s.Status.SwitchedAt, testConsoleDomain)
	}
	eventually(t, func() error {
		if got := hostnamesOf(getRoute(t, GatewayNamespace, consoleRoute)); !slices.Equal(got, []string{next}) {
			return fmt.Errorf("console route hostnames %v", got)
		}
		return nil
	})
	moved := getRoute(t, GatewayNamespace, consolePreviousRoute)
	// Pages redirect; the API keeps answering, so open pages learn of the move.
	if len(moved.Spec.Rules) != 2 || len(moved.Spec.Rules[1].Filters) != 1 {
		t.Fatalf("previous route rules = %+v", moved.Spec.Rules)
	}
	api, rr := moved.Spec.Rules[0], moved.Spec.Rules[1].Filters[0].RequestRedirect
	if hostnamesOf(moved)[0] != testConsoleDomain || rr == nil || rr.Hostname == nil || string(*rr.Hostname) != next {
		t.Errorf("previous route = %+v", moved.Spec)
	}
	if *api.Matches[0].Path.Value != "/api/" || string(api.BackendRefs[0].Name) != ConsoleService {
		t.Errorf("previous route API rule = %+v", api)
	}
	if got := hostnamesOf(getRoute(t, GatewayNamespace, consoleRedirectRoute)); !slices.Equal(got, []string{testConsoleDomain, next}) {
		t.Errorf("plain-HTTP redirect hostnames %v, want both names", got)
	}

	// A day later the old name is released.
	domainClock.Advance(previousGrace + time.Minute)
	touch(t)
	eventually(t, func() error {
		if listener(getGateway(t), ConsoleListenerName(testConsoleDomain)) != nil {
			return errors.New("old console listener still there")
		}
		err := k8s.Get(context.Background(), client.ObjectKey{Namespace: GatewayNamespace, Name: consolePreviousRoute}, &gwv1.HTTPRoute{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("previous route: %v", err)
		}
		return nil
	})
}

// touch changes an annotation on the settings to trigger a reconcile.
func touch(t *testing.T) {
	t.Helper()
	var s kwerftv1.ConsoleSettings
	if err := k8s.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		t.Fatal(err)
	}
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations["test/touched"] = time.Now().String()
	if err := k8s.Update(context.Background(), &s); err != nil {
		t.Fatal(err)
	}
}

func createClusterIssuer(t *testing.T) {
	t.Helper()
	ci := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "letsencrypt"},
		"spec": map[string]any{"acme": map[string]any{
			"server": "https://acme-staging-v02.api.letsencrypt.org/directory", "email": "ops@example.com",
		}},
	}}
	ci.SetGroupVersionKind(clusterIssuerGVK)
	if err := k8s.Create(context.Background(), ci); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

func dns01Settings(apps string) kwerftv1.ConsoleSettingsSpec {
	return kwerftv1.ConsoleSettingsSpec{AppsDomain: apps, TLS: kwerftv1.TLSDNS01, DNS01: &kwerftv1.DNS01Settings{Provider: "hetzner"}}
}

func TestWildcardListenerServesAppsUnderTheAppsDomain(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createClusterIssuer(t)
	projectNamespace(t, "wild")
	early := createDomain(t, "wild", "early", "early.apps.example.com")
	waitForDomain(t, early, "CertificatePending")
	useSettings(t, dns01Settings("apps.example.com"))

	// Kwerft's own DNS-01 Issuer and wildcard Certificate...
	issuer := &unstructured.Unstructured{}
	issuer.SetGroupVersionKind(issuerGVK)
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: DNSIssuer}, issuer)
	})
	solvers, _, _ := unstructured.NestedSlice(issuer.Object, "spec", "acme", "solvers")
	group, _, _ := unstructured.NestedString(solvers[0].(map[string]any), "dns01", "webhook", "groupName")
	tokenRef, _, _ := unstructured.NestedString(solvers[0].(map[string]any), "dns01", "webhook", "config", "tokenSecretKeyRef", "name")
	server, _, _ := unstructured.NestedString(issuer.Object, "spec", "acme", "server")
	if group != hetznerGroupName || tokenRef != DNSTokenSecret || server != "https://acme-staging-v02.api.letsencrypt.org/directory" {
		t.Errorf("issuer = %v", issuer.Object["spec"])
	}
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: WildcardSecret}, cert)
	})
	names, _, _ := unstructured.NestedStringSlice(cert.Object, "spec", "dnsNames")
	kind, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "kind")
	if !slices.Equal(names, []string{"*.apps.example.com"}) || kind != "Issuer" {
		t.Errorf("wildcard certificate spec = %v", cert.Object["spec"])
	}
	// ...the empty token Secret owners and admins fill in...
	var token corev1.Secret
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: DNSTokenSecret}, &token)
	})
	// ...and no shim certificate for the wildcard listener.
	if gw := getGateway(t); gw.Annotations["cert-manager.io/ignore-tls-listeners"] != WildcardListener {
		t.Errorf("gateway annotations = %v", gw.Annotations)
	}

	// Until the wildcard certificate exists, apps keep per-host listeners.
	if listener(getGateway(t), WildcardListener) != nil {
		t.Error("wildcard listener before its certificate is issued")
	}
	waitForListener(t, "early.apps.example.com", true)

	issue(t, WildcardSecret, "*.apps.example.com")
	var gw *gwv1.Gateway
	eventually(t, func() error {
		gw = getGateway(t)
		if listener(gw, WildcardListener) == nil {
			return errors.New("no wildcard listener")
		}
		return nil
	})
	l := listener(gw, WildcardListener)
	if string(*l.Hostname) != "*.apps.example.com" || string(l.TLS.CertificateRefs[0].Name) != WildcardSecret {
		t.Errorf("wildcard listener = %+v", l)
	}
	if ns := l.AllowedRoutes.Namespaces; *ns.From != gwv1.NamespacesFromSelector ||
		len(ns.Selector.MatchExpressions) != 1 || ns.Selector.MatchExpressions[0].Key != LabelProject {
		t.Errorf("wildcard listener admits %+v, want project namespaces", ns)
	}

	// The early Domain moves onto the wildcard: no listener of its own.
	waitForListener(t, "early.apps.example.com", false)
	early = waitForDomain(t, early, "CertificateIssued")
	if early.Status.Listener != WildcardListener {
		t.Errorf("early.status.listener = %q", early.Status.Listener)
	}

	// A new app under the apps domain attaches to the wildcard; one outside
	// it, or two labels down (not covered by *.), gets its own listener.
	app := createApp(t, "wild", "shop", imageApp("nginx:1.29"))
	app.Spec.Ports = []kwerftv1.AppPort{{Container: 8080, Public: "shop.apps.example.com"}}
	app.Spec.AllowFrom = nil
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	route := getRoute(t, "wild", "shop-8080")
	if sn := route.Spec.ParentRefs[0].SectionName; sn == nil || string(*sn) != WildcardListener {
		t.Errorf("shop route attaches to %v, want the wildcard listener", sn)
	}
	if listener(getGateway(t), ListenerName("shop.apps.example.com")) != nil {
		t.Error("an app under the wildcard got a listener of its own")
	}
	for _, host := range []string{"outside.example.org", "deep.shop.apps.example.com"} {
		createDomain(t, "wild", host, host)
		waitForListener(t, host, true)
	}

	settings := waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		if s.Status.WildcardDomain != "*.apps.example.com" {
			return fmt.Errorf("status.wildcardDomain = %q", s.Status.WildcardDomain)
		}
		return nil
	})
	if !slices.ContainsFunc(settings.Status.Certificates, func(c kwerftv1.CertificateState) bool {
		return c.Purpose == "apps-wildcard" && c.Ready
	}) {
		t.Errorf("status.certificates = %+v", settings.Status.Certificates)
	}

	// Turning DNS-01 off removes the wildcard; apps go back to their own listeners.
	var s kwerftv1.ConsoleSettings
	if err := k8s.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
		t.Fatal(err)
	}
	s.Spec = kwerftv1.ConsoleSettingsSpec{AppsDomain: "apps.example.com", TLS: kwerftv1.TLSHTTP01}
	if err := k8s.Update(ctx, &s); err != nil {
		t.Fatal(err)
	}
	waitForListener(t, "shop.apps.example.com", true)
	eventually(t, func() error {
		if listener(getGateway(t), WildcardListener) != nil {
			return errors.New("wildcard listener still there")
		}
		if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: DNSIssuer}, issuer); !apierrors.IsNotFound(err) {
			return fmt.Errorf("issuer still there (%v)", err)
		}
		return nil
	})
	route = getRoute(t, "wild", "shop-8080")
	eventually(t, func() error {
		route = getRoute(t, "wild", "shop-8080")
		if sn := route.Spec.ParentRefs[0].SectionName; string(*sn) != ListenerName("shop.apps.example.com") {
			return fmt.Errorf("route still attaches to %s", *sn)
		}
		return nil
	})
}

// Under the shared wildcard listener the Gateway admits every project, so
// isolation rests on Domain claims. A second project asking for a taken
// hostname, however it asks, ends up with no route for it.
func TestWildcardHostnameCannotBeHijacked(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	createClusterIssuer(t)
	useSettings(t, dns01Settings("hijack.example.com"))
	issue(t, WildcardSecret, "*.hijack.example.com")
	eventually(t, func() error {
		if listener(getGateway(t), WildcardListener) == nil {
			return errors.New("no wildcard listener")
		}
		return nil
	})
	const host = "shop.hijack.example.com"
	projectNamespace(t, "owner-proj")
	projectNamespace(t, "thief-proj")

	createApp(t, "owner-proj", "shop", kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx"}},
		Ports:  []kwerftv1.AppPort{{Container: 80, Public: host}},
	})
	ownerRoute := getRoute(t, "owner-proj", "shop-80")
	if string(*ownerRoute.Spec.ParentRefs[0].SectionName) != WildcardListener {
		t.Fatalf("owner route attaches to %s", *ownerRoute.Spec.ParentRefs[0].SectionName)
	}

	// 1. The same hostname on an App: its Domain loses, and no route appears.
	thief := createApp(t, "thief-proj", "shop", kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx"}},
		Ports:  []kwerftv1.AppPort{{Container: 80, Public: host}},
	})
	lost := waitForDomain(t, &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "thief-proj", Name: host}}, "HostnameConflict")
	if lost.Status.Listener != "" {
		t.Errorf("losing Domain reports listener %q", lost.Status.Listener)
	}
	waitForApp(t, thief, "Progressing")

	// 2. A hand-made Domain named after the hostname but claiming another
	// one: the App uses it, yet it holds a different hostname.
	decoy := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "thief-proj", Name: "shop2.hijack.example.com"},
		Spec: kwerftv1.DomainSpec{Hostname: "free.hijack.example.com"}}
	if err := k8s.Create(ctx, decoy); err != nil {
		t.Fatal(err)
	}
	waitForDomain(t, decoy, "CertificateIssued")
	thief2 := createApp(t, "thief-proj", "shop2", kwerftv1.AppSpec{
		Source: kwerftv1.AppSource{Image: &kwerftv1.ImageSource{Ref: "nginx"}},
		Ports:  []kwerftv1.AppPort{{Container: 80, Public: "shop2.hijack.example.com"}},
	})
	waitForApp(t, thief2, "Progressing")

	// 3. Renaming a won Domain's hostname to the taken one is refused.
	decoy.Spec.Hostname = host
	if err := k8s.Update(ctx, decoy); !apierrors.IsInvalid(err) {
		t.Errorf("changing a Domain's hostname: %v, want refused", err)
	}

	// Give the reconcilers time to (wrongly) act, then check: the thief's
	// project has no route for either hostname, the owner keeps its route.
	time.Sleep(time.Second)
	var routes gwv1.HTTPRouteList
	if err := k8s.List(ctx, &routes, client.InNamespace("thief-proj")); err != nil {
		t.Fatal(err)
	}
	for _, r := range routes.Items {
		t.Errorf("thief project has route %s for %v", r.Name, r.Spec.Hostnames)
	}
	getRoute(t, "owner-proj", "shop-80")
	if gw := getGateway(t); countHost(gw.Spec.Listeners, host) != 0 {
		t.Errorf("a listener of its own for %s", host)
	}
}

func TestSettingsReportPublicAddresses(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-addr"}}
	if err := k8s.Create(ctx, node); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), node) })
	node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.2"}, {Type: corev1.NodeExternalIP, Address: "203.0.113.24"}}
	if err := k8s.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	useSettings(t, kwerftv1.ConsoleSettingsSpec{})
	waitForSettings(t, func(s *kwerftv1.ConsoleSettings) error {
		if !slices.Equal(s.Status.PublicAddresses, []string{"203.0.113.24"}) {
			return fmt.Errorf("publicAddresses = %v", s.Status.PublicAddresses)
		}
		if s.Status.ConsoleDomain != testConsoleDomain {
			return fmt.Errorf("consoleDomain = %q, want the flag's", s.Status.ConsoleDomain)
		}
		return nil
	})
}

func TestSettingsValidation(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	other := &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: "second"}}
	expectInvalid(t, k8s.Create(ctx, other), "must be named kwerft")
	bad := &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName},
		Spec: kwerftv1.ConsoleSettingsSpec{TLS: kwerftv1.TLSDNS01}}
	expectInvalid(t, k8s.Create(ctx, bad), "tls dns01 needs appsDomain and dns01")
}
