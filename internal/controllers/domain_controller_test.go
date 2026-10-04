package controllers

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func TestListenerName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	hosts := []string{"api.example.com", "a-b.c.example.com", "a.b-c.example.com",
		"whoami.46.224.139.73.sslip.io", "a-very-long-subdomain-name-for-testing.apps.example.com"}
	seen := map[string]string{}
	for _, h := range hosts {
		n := ListenerName(h)
		if !valid.MatchString(n) || len(n) > 63 {
			t.Errorf("ListenerName(%q) = %q is not a valid section name", h, n)
		}
		if n != ListenerName(h) {
			t.Errorf("ListenerName(%q) is not stable", h)
		}
		if other, dup := seen[n]; dup {
			t.Errorf("%q and %q map to the same listener %q", h, other, n)
		}
		seen[n] = h
	}
}

func getGateway(t *testing.T) *gwv1.Gateway {
	t.Helper()
	var gw gwv1.Gateway
	eventually(t, func() error {
		return k8s.Get(context.Background(), client.ObjectKey{Namespace: GatewayNamespace, Name: GatewayName}, &gw)
	})
	return &gw
}

func listener(gw *gwv1.Gateway, name string) *gwv1.Listener {
	for i := range gw.Spec.Listeners {
		if string(gw.Spec.Listeners[i].Name) == name {
			return &gw.Spec.Listeners[i]
		}
	}
	return nil
}

// waitForListener waits until the Gateway has (or, with want=false, lacks)
// the listener for host, and returns the Gateway.
func waitForListener(t *testing.T, host string, want bool) *gwv1.Gateway {
	t.Helper()
	var gw *gwv1.Gateway
	eventually(t, func() error {
		gw = getGateway(t)
		if (listener(gw, ListenerName(host)) != nil) != want {
			return fmt.Errorf("listener for %s present=%v, want %v", host, !want, want)
		}
		return nil
	})
	return gw
}

func createDomain(t *testing.T, ns, name, host string) *kwerftv1.Domain {
	t.Helper()
	d := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}, Spec: kwerftv1.DomainSpec{Hostname: host}}
	if err := k8s.Create(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func waitForDomain(t *testing.T, d *kwerftv1.Domain, wantReason string) *kwerftv1.Domain {
	t.Helper()
	eventually(t, func() error {
		if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(d), d); err != nil {
			return err
		}
		reason, err := readyReason(d.Status.Conditions, d.Generation)
		if err != nil {
			return err
		}
		if reason != wantReason {
			return fmt.Errorf("reason %q, want %q", reason, wantReason)
		}
		return nil
	})
	return d
}

func TestGatewayHasHTTPAndConsoleListeners(t *testing.T) {
	requireEnvtest(t)
	gw := getGateway(t)
	if string(gw.Spec.GatewayClassName) != "traefik" {
		t.Errorf("gatewayClassName = %q", gw.Spec.GatewayClassName)
	}
	if gw.Annotations["cert-manager.io/cluster-issuer"] != "letsencrypt" {
		t.Errorf("cluster-issuer annotation = %q", gw.Annotations["cert-manager.io/cluster-issuer"])
	}
	if l := listener(gw, httpListener); l == nil || l.Port != 80 {
		t.Errorf("http listener = %+v", l)
	}
	l := listener(gw, ConsoleListenerName(testConsoleDomain))
	if l == nil || l.Hostname == nil || string(*l.Hostname) != testConsoleDomain || l.TLS == nil ||
		string(l.TLS.CertificateRefs[0].Name) != consoleSecretName(testConsoleDomain) {
		t.Fatalf("console listener = %+v", l)
	}
	if ns := l.AllowedRoutes.Namespaces; ns == nil || *ns.From != gwv1.NamespacesFromSame {
		t.Errorf("console listener admits %+v, want only its own namespace", ns)
	}

	// The console's routes come from the reconciler, not Helm.
	route := getRoute(t, GatewayNamespace, consoleRoute)
	if len(route.Spec.Hostnames) != 1 || string(route.Spec.Hostnames[0]) != testConsoleDomain ||
		string(*route.Spec.ParentRefs[0].SectionName) != ConsoleListenerName(testConsoleDomain) ||
		string(route.Spec.Rules[0].BackendRefs[0].Name) != ConsoleService {
		t.Errorf("console route = %+v", route.Spec)
	}
	redirect := getRoute(t, GatewayNamespace, consoleRedirectRoute)
	if string(*redirect.Spec.ParentRefs[0].SectionName) != httpListener || redirect.Spec.Rules[0].Filters[0].RequestRedirect == nil {
		t.Errorf("console redirect route = %+v", redirect.Spec)
	}
}

func getRoute(t *testing.T, ns, name string) *gwv1.HTTPRoute {
	t.Helper()
	var route gwv1.HTTPRoute
	eventually(t, func() error {
		return k8s.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &route)
	})
	return &route
}

func TestAppPublicPortGetsDomainHTTPSListenerAndRedirect(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "shopfront")
	host := "shop.example.com"
	app := createApp(t, "shopfront", "web", imageApp("nginx:1.29"))
	app.Spec.Ports = []kwerftv1.AppPort{{Container: 8080, Public: host}}
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}

	// The App claims a Domain...
	d := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: "shopfront", Name: host}}
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKeyFromObject(d), d) })
	if !metav1.IsControlledBy(d, app) || d.Spec.Hostname != host {
		t.Errorf("domain = %+v, want controlled by the App", d.ObjectMeta)
	}

	// ...which becomes an HTTPS listener only this project may attach to.
	gw := waitForListener(t, host, true)
	l := listener(gw, ListenerName(host))
	if l.Protocol != gwv1.HTTPSProtocolType || l.Port != 443 || string(*l.Hostname) != host {
		t.Errorf("listener = %+v", l)
	}
	ns := l.AllowedRoutes.Namespaces
	if ns == nil || *ns.From != gwv1.NamespacesFromSelector || ns.Selector.MatchLabels["kubernetes.io/metadata.name"] != "shopfront" {
		t.Errorf("allowedRoutes = %+v, want only namespace shopfront", ns)
	}
	if string(l.TLS.CertificateRefs[0].Name) != ListenerName(host)+"-tls" {
		t.Errorf("certificateRefs = %+v", l.TLS.CertificateRefs)
	}

	// Plain HTTP redirects to HTTPS.
	var redirect gwv1.HTTPRoute
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: "shopfront", Name: "web-8080-redirect"}, &redirect)
	})
	if sn := redirect.Spec.ParentRefs[0].SectionName; sn == nil || string(*sn) != httpListener {
		t.Errorf("redirect attaches to %v, want the http listener", sn)
	}
	f := redirect.Spec.Rules[0].Filters
	if len(f) != 1 || f[0].RequestRedirect == nil || *f[0].RequestRedirect.Scheme != "https" {
		t.Errorf("redirect filters = %+v", f)
	}

	d = waitForDomain(t, d, "CertificatePending")
	if d.Status.Listener != ListenerName(host) {
		t.Errorf("status.listener = %q", d.Status.Listener)
	}

	// Making the port private releases the Domain and its listener.
	if err := k8s.Get(ctx, client.ObjectKeyFromObject(app), app); err != nil {
		t.Fatal(err)
	}
	app.Spec.Ports = []kwerftv1.AppPort{{Container: 8080}}
	if err := k8s.Update(ctx, app); err != nil {
		t.Fatal(err)
	}
	waitForListener(t, host, false)
}

func TestDomainReadyWhenCertificateIssued(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "status-page")
	host := "status.example.com"
	d := createDomain(t, "status-page", "status", host)
	waitForDomain(t, d, "CertificatePending")

	// Play cert-manager: the gateway-shim names the Certificate after the secret.
	notAfter := time.Date(2027, 1, 2, 7, 49, 47, 0, time.UTC)
	cert := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": ListenerName(host) + "-tls", "namespace": GatewayNamespace},
		"status": map[string]any{
			"notAfter":   notAfter.Format(time.RFC3339),
			"conditions": []any{map[string]any{"type": "Ready", "status": "True", "message": "Certificate is up to date"}},
		},
	}}
	cert.SetGroupVersionKind(certificateGVK)
	if err := k8s.Create(context.Background(), cert); err != nil {
		t.Fatal(err)
	}

	d = waitForDomain(t, d, "CertificateIssued")
	if d.Status.NotAfter == nil || !d.Status.NotAfter.Equal(&metav1.Time{Time: notAfter}) {
		t.Errorf("notAfter = %v, want %v", d.Status.NotAfter, notAfter)
	}
}

func TestOlderDomainWinsHostname(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "first")
	projectNamespace(t, "second")
	host := "contested.example.com"
	first := createDomain(t, "first", "contested", host)
	waitForDomain(t, first, "CertificatePending")
	second := createDomain(t, "second", "contested", host)

	waitForDomain(t, second, "HostnameConflict")
	if second.Status.Listener != "" {
		t.Errorf("losing Domain reports listener %q", second.Status.Listener)
	}
	gw := waitForListener(t, host, true)
	if got := gw.Spec.Listeners; countHost(got, host) != 1 {
		t.Errorf("%d listeners for %s, want 1", countHost(got, host), host)
	}
	if sel := listener(gw, ListenerName(host)).AllowedRoutes.Namespaces.Selector.MatchLabels; sel["kubernetes.io/metadata.name"] != "first" {
		t.Errorf("listener belongs to %v, want project first", sel)
	}
}

func TestConsoleHostnameIsReserved(t *testing.T) {
	requireEnvtest(t)
	projectNamespace(t, "sneaky")
	d := createDomain(t, "sneaky", "console", testConsoleDomain)
	waitForDomain(t, d, "ReservedHostname")
	if n := countHost(getGateway(t).Spec.Listeners, testConsoleDomain); n != 1 {
		t.Errorf("%d listeners for the console hostname, want only the console's own", n)
	}
}

func countHost(ls []gwv1.Listener, host string) int {
	n := 0
	for _, l := range ls {
		if l.Hostname != nil && string(*l.Hostname) == host {
			n++
		}
	}
	return n
}

func TestUnusedCertificateSecretsGoAfterAGracePeriod(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	projectNamespace(t, "secret-sweep")
	t.Cleanup(domainClock.Reset)

	tlsSecret := func(name string, issued bool) *corev1.Secret {
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: GatewayNamespace},
			Type: corev1.SecretTypeTLS, Data: map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k")}}
		if issued {
			sec.Annotations = map[string]string{certManagerCertName: name}
		}
		if err := k8s.Create(ctx, sec); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k8s.Delete(ctx, sec) })
		return sec
	}
	live := tlsSecret(ListenerName("live.example.com")+"-tls", true)
	gone := tlsSecret(ListenerName("gone.example.com")+"-tls", true)
	handMade := tlsSecret(ListenerName("hand.example.com")+"-tls", false) // not cert-manager's
	other := tlsSecret("someone-elses-tls", true)                         // not a listener's

	get := func(sec *corev1.Secret) (*corev1.Secret, error) {
		var got corev1.Secret
		err := k8s.Get(ctx, client.ObjectKeyFromObject(sec), &got)
		return &got, err
	}
	d := createDomain(t, "secret-sweep", "live", "live.example.com")
	eventually(t, func() error {
		got, err := get(gone)
		if err != nil {
			return err
		}
		if got.Annotations[AnnotationUnusedSince] == "" {
			return fmt.Errorf("unused secret not marked: %v", got.Annotations)
		}
		return nil
	})
	for _, sec := range []*corev1.Secret{live, handMade, other} {
		if got, err := get(sec); err != nil || got.Annotations[AnnotationUnusedSince] != "" {
			t.Errorf("%s: %v %v", sec.Name, err, got.Annotations)
		}
	}

	// A week later (any Gateway change triggers the sweep), it is gone; the
	// others stay.
	domainClock.Advance(unusedSecretGrace + time.Hour)
	if err := k8s.Patch(ctx, d, client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"annotations":{"test/poke":"1"}}}`))); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if _, err := get(gone); !apierrors.IsNotFound(err) {
			return fmt.Errorf("unused secret still there: %v", err)
		}
		return nil
	})
	for _, sec := range []*corev1.Secret{live, handMade, other} {
		if _, err := get(sec); err != nil {
			t.Errorf("%s: %v", sec.Name, err)
		}
	}
}
