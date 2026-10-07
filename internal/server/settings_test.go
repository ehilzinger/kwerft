// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Settings against the test cluster with the chart's RBAC. No Domain
// reconciler runs here, so tests play it: they create the empty token Secret
// and write the settings' status.

const testHetznerToken = "hz-secret-token-0123456789abcdef"

// fakeDNS answers lookups from a table; anything else does not exist.
func fakeDNS(records map[string][]string) func(*settingsAPI) {
	return func(s *settingsAPI) {
		s.lookupHost = func(_ context.Context, host string) ([]string, error) {
			if a, ok := records[host]; ok {
				return a, nil
			}
			for name, a := range records {
				if strings.HasPrefix(name, "*.") && strings.HasSuffix(host, name[1:]) {
					return a, nil
				}
			}
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
	}
}

// fakeHetzner serves the Cloud API's zone lookup for one token and zone.
func fakeHetzner(t *testing.T, zone string) func(*settingsAPI) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testHetznerToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
			return
		}
		zones := []map[string]string{}
		if r.URL.Query().Get("name") == zone {
			zones = append(zones, map[string]string{"name": zone})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"zones": zones})
	}))
	t.Cleanup(srv.Close)
	return func(s *settingsAPI) { s.hetznerAPI = srv.URL }
}

func withSettings(hooks ...func(*settingsAPI)) func(*Config) {
	return func(c *Config) {
		c.ConsoleDomain = "console.example.com"
		c.settingsHook = func(s *settingsAPI) {
			for _, h := range hooks {
				h(s)
			}
		}
	}
}

// settingsFixture creates what the Domain reconciler would: the empty token
// Secret and settings whose status names the server's address. Removed again
// afterwards, since the settings are a cluster-wide singleton.
func settingsFixture(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: controllers.DNSTokenSecret}}
	if err := cluster.admin.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	cs := &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}}
	if err := cluster.admin.Create(ctx, cs); err != nil {
		t.Fatal(err)
	}
	cs.Status = kwerftv1.ConsoleSettingsStatus{ConsoleDomain: "console.example.com", PublicAddresses: []string{"203.0.113.24"}}
	if err := cluster.admin.Status().Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cluster.admin.Delete(context.Background(), sec)
		_ = cluster.admin.Delete(context.Background(), cs)
		eventually(t, func() error {
			return ignoreGone(cluster.admin.Get(context.Background(), client.ObjectKeyFromObject(cs), &kwerftv1.ConsoleSettings{}))
		})
	})
}

func ignoreGone(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		return errors.New("still there")
	}
	return err
}

// canName is can() for one named object.
func canName(t *testing.T, role, namespace, verb, group, resource, sub, name string) bool {
	t.Helper()
	cs, err := cluster.imp.Clientset(role+"@example.com", role)
	if err != nil {
		t.Fatal(err)
	}
	review, err := cs.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{ResourceAttributes: &authorizationv1.ResourceAttributes{
			Namespace: namespace, Verb: verb, Group: group, Resource: resource, Subresource: sub, Name: name,
		}},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return review.Status.Allowed
}

func settingsNow(t *testing.T) *kwerftv1.ConsoleSettings {
	t.Helper()
	var cs kwerftv1.ConsoleSettings
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs); err != nil {
		t.Fatal(err)
	}
	return &cs
}

func TestSettingsOnlyOwnersAndAdminsChangeThem(t *testing.T) {
	c := newConsole(t, withSettings(fakeDNS(map[string][]string{"ops.example.net": {"203.0.113.24"}}), fakeHetzner(t, "example.com")))
	settingsFixture(t)

	// Everyone reads them: the deploy wizard suggests hostnames.
	for _, s := range []*session{c.owner, c.dev, c.viewer} {
		var view settingsJSON
		if code := s.do(t, "GET", "/api/v1/settings", nil, &view); code != http.StatusOK || view.ConsoleDomain != "console.example.com" {
			t.Errorf("read settings: %d %+v", code, view)
		}
	}
	for _, s := range []*session{c.dev, c.viewer} {
		if code := s.do(t, "PUT", "/api/v1/settings/console-domain", map[string]string{"hostname": "ops.example.net", "confirm": "ops.example.net"}, nil); code != http.StatusForbidden {
			t.Errorf("console domain change: %d, want 403", code)
		}
		if code := s.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.com", "tls": "dns01", "token": testHetznerToken}, nil); code != http.StatusForbidden {
			t.Errorf("apps domain change: %d, want 403", code)
		}
		if code := s.do(t, "POST", "/api/v1/settings/dns-check", map[string]string{"hostname": "ops.example.net"}, nil); code != http.StatusForbidden {
			t.Errorf("dns check: %d, want 403", code)
		}
		if code := s.do(t, "GET", "/api/v1/settings/passkeys", nil, nil); code != http.StatusForbidden {
			t.Errorf("passkey holders: %d, want 403", code)
		}
	}
	if settingsNow(t).Spec.AppsDomain != "" {
		t.Error("a refused request changed the settings")
	}

	// Kubernetes agrees, should the console's own check ever be wrong.
	for _, check := range []struct {
		role, verb, group, res, sub, ns string
		want                            bool
	}{
		{"owner", "patch", "kwerft.dev", "consolesettings", "", "", true},
		{"admin", "patch", "kwerft.dev", "consolesettings", "", "", true},
		{"developer", "patch", "kwerft.dev", "consolesettings", "", "", false},
		{"viewer", "patch", "kwerft.dev", "consolesettings", "", "", false},
		// The DNS token: owners and admins write it, nobody reads it.
		{"owner", "patch", "", "secrets", "", controllers.GatewayNamespace, true},
		{"admin", "patch", "", "secrets", "", controllers.GatewayNamespace, true},
		{"owner", "get", "", "secrets", "", controllers.GatewayNamespace, false},
		{"developer", "patch", "", "secrets", "", controllers.GatewayNamespace, false},
		// Domain claims decide who serves a hostname under the wildcard, so
		// neither their status nor routes are anyone's to write directly.
		{"developer", "patch", "kwerft.dev", "domains", "status", "shop", false},
		{"developer", "create", "gateway.networking.k8s.io", "httproutes", "", "shop", false},
		{"admin", "create", "gateway.networking.k8s.io", "httproutes", "", "shop", false},
	} {
		got := canName(t, check.role, check.ns, check.verb, check.group, check.res, check.sub, tokenNameFor(check.res))
		if got != check.want {
			t.Errorf("%s may %s %s/%s in %q: %v, want %v", check.role, check.verb, check.res, check.sub, check.ns, got, check.want)
		}
	}
}

func TestSettingsTokenRoleCoversOnlyTheTokenSecret(t *testing.T) {
	requireCluster(t)
	for _, verb := range []string{"patch", "get", "create", "update"} {
		if canName(t, "owner", controllers.GatewayNamespace, verb, "", "secrets", "", "kwerft-data-key") {
			t.Errorf("owner may %s the data key Secret", verb)
		}
	}
}

func tokenNameFor(res string) string {
	if res == "secrets" {
		return controllers.DNSTokenSecret
	}
	return ""
}

func TestSettingsDNSTokenIsWriteOnly(t *testing.T) {
	c := newConsole(t, withSettings(fakeDNS(nil), fakeHetzner(t, "example.com")))
	settingsFixture(t)

	// A token Hetzner rejects is not stored.
	var bad apiError
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.com", "tls": "dns01", "token": "wrong"}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("rejected token: %d %+v", code, bad)
	}
	// DNS-01 without any token is refused.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.com", "tls": "dns01"}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("dns01 without a token: %d %+v", code, bad)
	}
	// A zone the token's project does not have.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.org", "tls": "dns01", "token": testHetznerToken}, &bad); code != http.StatusBadRequest || bad.Field != "appsDomain" {
		t.Errorf("missing zone: %d %+v", code, bad)
	}

	res := c.owner.raw(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "Apps.Example.com", "tls": "dns01", "token": testHetznerToken})
	if res.code != http.StatusOK || strings.Contains(res.body, testHetznerToken) {
		t.Fatalf("save: %d %s", res.code, res.body)
	}
	var saved struct {
		Settings settingsJSON `json:"settings"`
		Zone     string       `json:"zone"`
	}
	_ = json.Unmarshal([]byte(res.body), &saved)
	if saved.Zone != "example.com" || saved.Settings.AppsDomain != "apps.example.com" || saved.Settings.TLS != "dns01" || !saved.Settings.TokenSet {
		t.Errorf("saved = %+v", saved)
	}
	cs := settingsNow(t)
	if cs.Spec.AppsDomain != "apps.example.com" || cs.Spec.TLS != kwerftv1.TLSDNS01 || cs.Spec.DNS == nil || cs.Spec.DNS.Provider != "hetzner" || cs.Spec.DNS.ManageRecords ||
		cs.Annotations[controllers.AnnotationDNSTokenUpdated] == "" {
		t.Errorf("settings = %+v %v", cs.Spec, cs.Annotations)
	}
	var sec corev1.Secret
	if err := cluster.admin.Get(context.Background(), client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: controllers.DNSTokenSecret}, &sec); err != nil {
		t.Fatal(err)
	}
	if string(sec.Data[controllers.DNSTokenKey]) != testHetznerToken {
		t.Errorf("stored token = %q", sec.Data[controllers.DNSTokenKey])
	}

	// No read path returns it, for any role, and the audit log only says it changed.
	for _, s := range []*session{c.owner, c.dev, c.viewer} {
		if got := s.raw(t, "GET", "/api/v1/settings", nil); strings.Contains(got.body, testHetznerToken) {
			t.Errorf("GET /settings returns the token: %s", got.body)
		}
	}
	entries, err := c.store.RecentAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
		if strings.Contains(e.Target+e.Detail, testHetznerToken) {
			t.Errorf("audit entry carries the token: %+v", e)
		}
	}
	if !strings.Contains(strings.Join(actions, " "), "settings.dns_token") || !strings.Contains(strings.Join(actions, " "), "settings.apps") {
		t.Errorf("audit actions = %v", actions)
	}

	// Saving again without a token keeps the stored one.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.com", "tls": "dns01"}, nil); code != http.StatusOK {
		t.Errorf("save without token: %d", code)
	}
	// Back to HTTP-01 clears the solver.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]string{"appsDomain": "apps.example.com", "tls": "http01"}, nil); code != http.StatusOK {
		t.Errorf("http01: %d", code)
	}
	if cs := settingsNow(t); cs.Spec.TLS != kwerftv1.TLSHTTP01 || cs.Spec.DNS != nil {
		t.Errorf("after http01: %+v", cs.Spec)
	}
}

func TestSettingsManagedRecords(t *testing.T) {
	c := newConsole(t, withSettings(fakeDNS(nil), fakeHetzner(t, "example.org")))
	settingsFixture(t)
	ctx := context.Background()

	// Managed records need a token, whatever the certificate method.
	var bad apiError
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]any{"appsDomain": "apps.example.org", "tls": "http01", "manageRecords": true}, &bad); code != http.StatusBadRequest || bad.Field != "token" {
		t.Errorf("records without a token: %d %+v", code, bad)
	}

	// The console's hostname (console.example.com) is not in the token's
	// project: saved, with a warning that its record stays manual.
	var saved struct {
		Settings settingsJSON `json:"settings"`
		Zone     string       `json:"zone"`
		Warning  string       `json:"warning"`
	}
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]any{"appsDomain": "apps.example.org", "tls": "http01", "manageRecords": true, "token": testHetznerToken}, &saved); code != http.StatusOK {
		t.Fatalf("save: %d", code)
	}
	if saved.Zone != "example.org" || !strings.Contains(saved.Warning, "console.example.com") || !saved.Settings.ManageRecords || saved.Settings.TLS != "http01" {
		t.Errorf("saved = %+v", saved)
	}
	cs := settingsNow(t)
	if cs.Spec.DNS == nil || !cs.Spec.DNS.ManageRecords || cs.Spec.DNS.Provider != "hetzner" || cs.Spec.TLS != kwerftv1.TLSHTTP01 {
		t.Errorf("spec = %+v", cs.Spec)
	}

	// What the DNS reconciler reports shows up on the Settings page.
	cs.Status.DNS = &kwerftv1.DNSStatus{Zones: []string{"example.org"}, Records: []kwerftv1.DNSRecordStatus{
		{Hostname: "*.apps.example.org", Purpose: "apps", Zone: "example.org", State: kwerftv1.DNSManaged, Values: []string{"203.0.113.24"}},
		{Hostname: "console.example.com", Purpose: "console", State: kwerftv1.DNSNoZone, Message: "create its records by hand"},
	}}
	if err := cluster.admin.Status().Update(ctx, cs); err != nil {
		t.Fatal(err)
	}
	var view settingsJSON
	if code := c.viewer.do(t, "GET", "/api/v1/settings", nil, &view); code != http.StatusOK {
		t.Fatalf("get: %d", code)
	}
	if len(view.DNSRecords) != 2 || view.DNSRecords[0].State != "Managed" || view.DNSRecords[1].Values == nil {
		t.Errorf("dnsRecords = %+v", view.DNSRecords)
	}

	// A name without records in a managed zone is fine: Kwerft creates it,
	// so the console can move there. Elsewhere it is still missing.
	var dns dnsResult
	if code := c.owner.do(t, "POST", "/api/v1/settings/dns-check", map[string]string{"hostname": "ops.example.org"}, &dns); code != http.StatusOK || !dns.OK || !dns.Managed {
		t.Errorf("managed zone: %d %+v", code, dns)
	}
	dns = dnsResult{}
	if code := c.owner.do(t, "POST", "/api/v1/settings/dns-check", map[string]string{"hostname": "ops.example.net"}, &dns); code != http.StatusOK || dns.OK || dns.Managed {
		t.Errorf("other zone: %d %+v", code, dns)
	}
	var moved settingsJSON
	if code := c.owner.do(t, "PUT", "/api/v1/settings/console-domain", map[string]string{"hostname": "ops.example.org", "confirm": "ops.example.org"}, &moved); code != http.StatusOK || moved.PendingConsoleDomain != "ops.example.org" {
		t.Errorf("move into a managed zone: %d %+v", code, moved)
	}

	// Off again, with HTTP-01: no provider left, and no stale report.
	if code := c.owner.do(t, "PUT", "/api/v1/settings/apps", map[string]any{"appsDomain": "apps.example.org", "tls": "http01", "manageRecords": false}, &saved); code != http.StatusOK {
		t.Fatalf("off: %d", code)
	}
	if saved.Settings.ManageRecords || len(saved.Settings.DNSRecords) != 0 {
		t.Errorf("after off: %+v", saved.Settings)
	}
	if cs := settingsNow(t); cs.Spec.DNS != nil {
		t.Errorf("spec.dns kept: %+v", cs.Spec.DNS)
	}
}

func TestSettingsConsoleDomainMove(t *testing.T) {
	c := newConsole(t, withSettings(fakeDNS(map[string][]string{
		"ops.example.net":    {"203.0.113.24"},
		"wrong.example.net":  {"198.51.100.7"},
		"*.apps.example.net": {"203.0.113.24"},
	})))
	settingsFixture(t)
	ctx := context.Background()

	// DNS check for the console name and for a wildcard.
	var dns dnsResult
	if code := c.owner.do(t, "POST", "/api/v1/settings/dns-check", map[string]any{"hostname": "ops.example.net"}, &dns); code != http.StatusOK || !dns.OK {
		t.Errorf("dns check: %d %+v", code, dns)
	}
	if c.owner.do(t, "POST", "/api/v1/settings/dns-check", map[string]any{"hostname": "apps.example.net", "wildcard": true}, &dns); !dns.OK ||
		!strings.HasSuffix(dns.Hostname, ".apps.example.net") {
		t.Errorf("wildcard dns check: %+v", dns)
	}
	if c.owner.do(t, "POST", "/api/v1/settings/dns-check", map[string]any{"hostname": "missing.example.net"}, &dns); dns.OK {
		t.Errorf("missing record passes: %+v", dns)
	}

	var bad apiError
	move := func(host, confirm string) int {
		bad = apiError{}
		return c.owner.do(t, "PUT", "/api/v1/settings/console-domain", map[string]string{"hostname": host, "confirm": confirm}, &bad)
	}
	if code := move("wrong.example.net", "wrong.example.net"); code != http.StatusUnprocessableEntity || !strings.Contains(bad.Error, "198.51.100.7") {
		t.Errorf("wrong address: %d %+v", code, bad)
	}
	if code := move("ops.example.net", "ops.example.com"); code != http.StatusBadRequest || bad.Field != "confirm" {
		t.Errorf("unconfirmed: %d %+v", code, bad)
	}

	// Someone whose only second factor is a passkey would be locked out.
	pw, _ := auth.HashPassword("a long test password")
	mara := &store.User{Email: "mara@example.com", Name: "Mara", PasswordHash: pw, Role: store.RoleDeveloper}
	if err := c.store.CreateUser(ctx, mara); err != nil {
		t.Fatal(err)
	}
	if err := c.store.AddPasskey(ctx, &store.Passkey{UserID: mara.ID, CredentialID: []byte("cred-1"), Name: "laptop", Credential: []byte("{}")}); err != nil {
		t.Fatal(err)
	}
	var holders []passkeyHolderJSON
	if code := c.owner.do(t, "GET", "/api/v1/settings/passkeys", nil, &holders); code != http.StatusOK || len(holders) != 1 || !holders[0].Stranded {
		t.Errorf("passkey holders: %d %+v", code, holders)
	}
	if code := move("ops.example.net", "ops.example.net"); code != http.StatusConflict || !strings.Contains(bad.Error, "mara@example.com") {
		t.Errorf("stranded member: %d %+v", code, bad)
	}
	if settingsNow(t).Spec.ConsoleDomain != "" {
		t.Fatal("a refused move changed the settings")
	}

	// With recovery codes she keeps a way in, and the move goes ahead.
	if err := c.store.ReplaceRecoveryCodes(ctx, mara.ID, []string{auth.HashToken("code-1")}); err != nil {
		t.Fatal(err)
	}
	var view settingsJSON
	if code := c.owner.do(t, "PUT", "/api/v1/settings/console-domain", map[string]string{"hostname": "OPS.example.net.", "confirm": "ops.example.net"}, &view); code != http.StatusOK {
		t.Fatalf("move: %d", code)
	}
	if view.ConsoleDomain != "console.example.com" || view.PendingConsoleDomain != "ops.example.net" {
		t.Errorf("after the request: %+v (the console moves once the certificate is issued)", view)
	}
	if got := settingsNow(t).Spec.ConsoleDomain; got != "ops.example.net" {
		t.Errorf("spec.consoleDomain = %q", got)
	}
	entries, _ := c.store.RecentAudit(ctx, 10)
	if len(entries) == 0 || entries[0].Action != "settings.console_domain" || entries[0].Target != "ops.example.net" || entries[0].Detail != "from console.example.com" {
		t.Errorf("audit = %+v", entries[:min(len(entries), 1)])
	}

	// Moving back to the current name cancels, even with someone stranded.
	if err := c.store.DeleteRecoveryCodes(ctx, mara.ID); err != nil {
		t.Fatal(err)
	}
	if code := move("console.example.com", "console.example.com"); code != http.StatusOK {
		t.Errorf("cancel: %d %+v", code, bad)
	}
	if got := settingsNow(t).Spec.ConsoleDomain; got != "console.example.com" {
		t.Errorf("after cancel spec.consoleDomain = %q", got)
	}
	if entries, _ = c.store.RecentAudit(ctx, 1); entries[0].Detail != "cancelled the move to ops.example.net" {
		t.Errorf("cancel audit = %+v", entries[0])
	}
}

type rawResponse struct {
	code int
	body string
}

func (s *session) raw(t *testing.T, method, path string, body any) rawResponse {
	t.Helper()
	var m json.RawMessage
	code := s.do(t, method, path, body, &m)
	return rawResponse{code, string(m)}
}
