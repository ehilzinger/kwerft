package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Settings: the console's hostname and the apps base domain with its
// certificate method, stored in the ConsoleSettings singleton (see
// api/v1alpha1) that the Domain reconciler applies.
//
// Everyone signed in may read them (the deploy wizard suggests hostnames
// under the apps domain); owners and admins change them. Writes go through
// impersonation like every other write, so Kubernetes RBAC decides as well:
// only owners and admins may write kwerft.dev ConsoleSettings, and only they
// may patch — never read — the DNS token Secret (roles.yaml). The token is
// write-only: no endpoint returns it, and the audit log records only that it
// changed.

type settingsAPI struct {
	*api
	lookupHost func(ctx context.Context, host string) ([]string, error)
	hetznerAPI string
	http       *http.Client
}

func (a *api) registerSettings(mux *http.ServeMux) {
	s := &settingsAPI{api: a, lookupHost: net.DefaultResolver.LookupHost, hetznerAPI: hetzner.CloudAPI,
		http: &http.Client{Timeout: 15 * time.Second}}
	if a.cfg.settingsHook != nil {
		a.cfg.settingsHook(s)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return a.sameOrigin(read(a.requireRole(h, store.RoleOwner, store.RoleAdmin)))
	}
	mux.HandleFunc("GET /api/v1/settings", read(s.get))
	mux.HandleFunc("POST /api/v1/settings/dns-check", admin(s.dnsCheck))
	mux.HandleFunc("GET /api/v1/settings/passkeys", read(a.requireRole(s.passkeyHolders, store.RoleOwner, store.RoleAdmin)))
	mux.HandleFunc("PUT /api/v1/settings/console-domain", admin(s.setConsoleDomain))
	mux.HandleFunc("PUT /api/v1/settings/apps", admin(s.setApps))
}

// ---- reading -----------------------------------------------------------------

type certificateJSON struct {
	Name      string     `json:"name"`
	Purpose   string     `json:"purpose"`
	Hostnames []string   `json:"hostnames"`
	State     string     `json:"state"` // valid | issuing | failed
	Message   string     `json:"message,omitempty"`
	NotAfter  *time.Time `json:"notAfter,omitempty"`
}

type settingsJSON struct {
	ConsoleDomain         string            `json:"consoleDomain"`
	PendingConsoleDomain  string            `json:"pendingConsoleDomain,omitempty"`
	PreviousConsoleDomain string            `json:"previousConsoleDomain,omitempty"`
	AppsDomain            string            `json:"appsDomain,omitempty"`
	TLS                   string            `json:"tls"`
	DNSProvider           string            `json:"dnsProvider,omitempty"`
	TokenSet              bool              `json:"tokenSet"`
	ManageRecords         bool              `json:"manageRecords"`
	DNSRecords            []dnsRecordJSON   `json:"dnsRecords"`
	DNSMessage            string            `json:"dnsMessage,omitempty"`
	DNSSyncedAt           *time.Time        `json:"dnsSyncedAt,omitempty"`
	WildcardDomain        string            `json:"wildcardDomain,omitempty"`
	PublicAddresses       []string          `json:"publicAddresses"`
	Certificates          []certificateJSON `json:"certificates"`
	Ready                 *conditionJSON    `json:"ready,omitempty"`
}

// dnsRecordJSON is one hostname whose records Kwerft keeps (status.dns).
type dnsRecordJSON struct {
	Hostname string   `json:"hostname"`
	Purpose  string   `json:"purpose"`
	Zone     string   `json:"zone,omitempty"`
	State    string   `json:"state"` // Managed | External | Conflict | TakenOver | NoZone | Error
	Values   []string `json:"values"`
	Message  string   `json:"message,omitempty"`
}

type conditionJSON struct {
	Status  bool   `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// load reads the settings as the user; nil when none exist yet.
func (s *settingsAPI) load(ctx context.Context, c client.Client) (*kwerftv1.ConsoleSettings, error) {
	var cs kwerftv1.ConsoleSettings
	err := c.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &cs, nil
}

func (s *settingsAPI) view(cs *kwerftv1.ConsoleSettings) settingsJSON {
	out := settingsJSON{ConsoleDomain: s.consoleDomain(), TLS: string(kwerftv1.TLSHTTP01),
		PublicAddresses: []string{}, Certificates: []certificateJSON{}, DNSRecords: []dnsRecordJSON{}}
	if cs == nil {
		return out
	}
	st := cs.Status
	if st.ConsoleDomain != "" {
		out.ConsoleDomain = st.ConsoleDomain
	}
	if d := cs.Spec.ConsoleDomain; d != "" && d != out.ConsoleDomain {
		out.PendingConsoleDomain = d
	}
	out.PreviousConsoleDomain = st.PreviousConsoleDomain
	out.AppsDomain = cs.Spec.AppsDomain
	if cs.Spec.TLS != "" {
		out.TLS = string(cs.Spec.TLS)
	}
	if cs.Spec.DNS != nil {
		out.DNSProvider = cs.Spec.DNS.Provider
		out.ManageRecords = cs.Spec.DNS.ManageRecords
	}
	if d := st.DNS; d != nil && out.ManageRecords {
		for _, rec := range d.Records {
			values := rec.Values
			if values == nil {
				values = []string{}
			}
			out.DNSRecords = append(out.DNSRecords, dnsRecordJSON{Hostname: rec.Hostname, Purpose: rec.Purpose, Zone: rec.Zone,
				State: string(rec.State), Values: values, Message: rec.Message})
		}
		out.DNSMessage = d.Message
		out.DNSSyncedAt = timePtr(d.SyncedAt)
	}
	out.TokenSet = cs.Annotations[controllers.AnnotationDNSTokenUpdated] != ""
	out.WildcardDomain = st.WildcardDomain
	if st.PublicAddresses != nil {
		out.PublicAddresses = st.PublicAddresses
	}
	for _, c := range st.Certificates {
		cj := certificateJSON{Name: c.Name, Purpose: c.Purpose, Hostnames: c.Hostnames, Message: c.Message, NotAfter: timePtr(c.NotAfter)}
		switch {
		case c.Ready:
			cj.State = "valid"
		case c.Reason == "CertificateIssuing" || c.Reason == "CertificatePending" || c.Reason == "CertificateUnknown":
			cj.State = "issuing"
		default:
			cj.State = "failed"
		}
		out.Certificates = append(out.Certificates, cj)
	}
	if c := meta.FindStatusCondition(st.Conditions, controllers.ConditionReady); c != nil && c.ObservedGeneration == cs.Generation {
		out.Ready = &conditionJSON{Status: c.Status == metav1.ConditionTrue, Reason: c.Reason, Message: c.Message}
	}
	return out
}

func (s *settingsAPI) get(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.read", "settings", "Settings not found.", err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(cs))
}

// ---- DNS ---------------------------------------------------------------------

func normalizeHost(h string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
}

func validHostname(h string) bool { return len(h) <= 253 && hostnameRE.MatchString(h) }

type dnsResult struct {
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
	Expected  []string `json:"expected"`
	OK        bool     `json:"ok"`
	Message   string   `json:"message"`
	// Managed: the name has no record yet, but Kwerft creates it (the zone
	// is one whose records it manages).
	Managed bool `json:"managed,omitempty"`

	notFound bool
}

// managedZone is the zone Kwerft keeps records in that contains host, or "".
func managedZone(cs *kwerftv1.ConsoleSettings, host string) string {
	if cs == nil || cs.Spec.DNS == nil || !cs.Spec.DNS.ManageRecords || cs.Status.DNS == nil {
		return ""
	}
	best := ""
	for _, z := range cs.Status.DNS.Zones {
		if (host == z || strings.HasSuffix(host, "."+z)) && len(z) > len(best) {
			best = z
		}
	}
	return best
}

// checkManaged is checkDNS, except that a name without records counts as
// fine when Kwerft is about to create them.
func (s *settingsAPI) checkManaged(ctx context.Context, cs *kwerftv1.ConsoleSettings, host string) dnsResult {
	view := s.view(cs)
	res := s.checkDNS(ctx, host, view.PublicAddresses)
	if zone := managedZone(cs, host); !res.OK && len(res.Addresses) == 0 && res.notFound && zone != "" {
		res.OK, res.Managed = true, true
		res.Message = "Kwerft creates the record in the Hetzner zone " + zone + "."
	}
	return res
}

// checkDNS resolves host and compares the answer with the server's public
// addresses (reported by the Domain reconciler from the nodes).
func (s *settingsAPI) checkDNS(ctx context.Context, host string, expected []string) dnsResult {
	res := dnsResult{Hostname: host, Addresses: []string{}, Expected: expected}
	if expected == nil {
		res.Expected = []string{}
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := s.lookupHost(ctx, host)
	var dnsErr *net.DNSError
	switch {
	case errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		res.Message = host + " has no DNS record yet."
		res.notFound = true
		return res
	case err != nil:
		res.Message = "Could not look up " + host + ": " + err.Error()
		return res
	}
	slices.Sort(addrs)
	res.Addresses = addrs
	if len(expected) == 0 {
		res.Message = "This server's public address is not known yet. Try again in a moment."
		return res
	}
	for _, a := range addrs {
		if slices.Contains(expected, a) {
			res.OK = true
			res.Message = host + " points to this server (" + a + ")."
			return res
		}
	}
	res.Message = host + " points to " + strings.Join(addrs, ", ") + ", not to this server (" + strings.Join(expected, ", ") + ")."
	return res
}

func (s *settingsAPI) dnsCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname string `json:"hostname"`
		Wildcard bool   `json:"wildcard"` // check *.<hostname> through a random name below it
	}
	if !decode(w, r, &req) {
		return
	}
	host := normalizeHost(req.Hostname)
	if !validHostname(host) {
		writeFieldError(w, "hostname", "Enter a hostname, like ops.example.com.")
		return
	}
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.read", "settings", "Settings not found.", err)
		return
	}
	if req.Wildcard {
		host = "kwerft-check-" + strings.ToLower(auth.NewToken()[:8]) + "." + host
	}
	writeJSON(w, http.StatusOK, s.checkManaged(ctx, cs, host))
}

// ---- console hostname ----------------------------------------------------------

type passkeyHolderJSON struct {
	Email         string `json:"email"`
	Name          string `json:"name"`
	Passkeys      int    `json:"passkeys"`
	TOTP          bool   `json:"totp"`
	RecoveryCodes int    `json:"recoveryCodes"`
	Stranded      bool   `json:"stranded"` // no other way to finish signing in
}

func (s *settingsAPI) holders(ctx context.Context) ([]passkeyHolderJSON, error) {
	list, err := s.store.PasskeyHolders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]passkeyHolderJSON, 0, len(list))
	for _, h := range list {
		out = append(out, passkeyHolderJSON{Email: h.Email, Name: h.Name, Passkeys: h.Passkeys, TOTP: h.TOTP,
			RecoveryCodes: h.RecoveryCodes, Stranded: h.StrandedByMove()})
	}
	return out, nil
}

// passkeyHolders tells the Settings page who is affected by a move: passkeys
// only work on the hostname they were created on.
func (s *settingsAPI) passkeyHolders(w http.ResponseWriter, r *http.Request) {
	out, err := s.holders(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// setConsoleDomain asks for the console to move. The reconciler serves the
// new name next to the old one and switches once its certificate is issued;
// the page polls GET /settings and sends the browser over then.
func (s *settingsAPI) setConsoleDomain(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Hostname string `json:"hostname"`
		Confirm  string `json:"confirm"` // the hostname typed again: passkeys and sessions do not move along
	}
	if !decode(w, r, &req) {
		return
	}
	host := normalizeHost(req.Hostname)
	if !validHostname(host) {
		writeFieldError(w, "hostname", "Enter a hostname, like ops.example.com.")
		return
	}
	if normalizeHost(req.Confirm) != host {
		writeFieldError(w, "confirm", "Type the new hostname again to confirm.")
		return
	}
	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.console_domain", host, "Settings not found.", err)
		return
	}
	view := s.view(cs)
	if host == view.ConsoleDomain && view.PendingConsoleDomain == "" {
		writeFieldError(w, "hostname", "The console already uses "+host+".")
		return
	}
	if host == view.AppsDomain {
		writeFieldError(w, "hostname", host+" is the apps domain. Choose a name of its own for the console.")
		return
	}
	if taken, err := s.hostnameInUse(ctx, c, host); err != nil {
		s.kubeError(w, r, p, "settings.console_domain", host, "Domains not found.", err)
		return
	} else if taken != "" {
		writeFieldError(w, "hostname", host+" is already used by "+taken+".")
		return
	}
	// Back to the current name undoes a pending move: nothing to check.
	undo := host == view.ConsoleDomain
	if !undo {
		if dns := s.checkManaged(ctx, cs, host); !dns.OK {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": dns.Message, "field": "hostname", "dns": dns})
			return
		}
	}
	// Everyone must keep a way in: after the move passkeys stop working, so
	// whoever has nothing else (authenticator app, recovery codes) would be
	// locked out. They add one first.
	holders, err := s.holders(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	var stranded []string
	for _, h := range holders {
		if h.Stranded {
			stranded = append(stranded, h.Email)
		}
	}
	if len(stranded) > 0 && !undo {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "These members could not sign in after the move, because their passkeys only work on the current hostname and they have " +
				"no authenticator app or recovery codes: " + strings.Join(stranded, ", ") + ". They need to add an authenticator app or new recovery codes first.",
			"stranded": stranded,
		})
		return
	}

	patch := map[string]any{"spec": map[string]any{"consoleDomain": host}}
	if err := s.patchSettings(ctx, c, cs, patch); err != nil {
		s.kubeError(w, r, p, "settings.console_domain", host, "Settings not found.", err)
		return
	}
	detail := "from " + view.ConsoleDomain
	if undo {
		detail = "cancelled the move to " + view.PendingConsoleDomain
	}
	s.audit(r, p.user.Email, "settings.console_domain", host, detail)
	cs, err = s.load(ctx, c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(cs))
}

// hostnameInUse names the project/Domain already claiming host, if any.
func (s *settingsAPI) hostnameInUse(ctx context.Context, c client.Client, host string) (string, error) {
	var list kwerftv1.DomainList
	if err := s.list(ctx, c, &list); err != nil {
		return "", err
	}
	for _, d := range list.Items {
		if d.Spec.Hostname == host {
			return "the project " + d.Namespace, nil
		}
	}
	return "", nil
}

// patchSettings merge-patches the singleton, creating it first if needed.
func (s *settingsAPI) patchSettings(ctx context.Context, c client.Client, cs *kwerftv1.ConsoleSettings, patch map[string]any) error {
	if cs == nil {
		cs = &kwerftv1.ConsoleSettings{ObjectMeta: metav1.ObjectMeta{Name: kwerftv1.ConsoleSettingsName}}
		if err := c.Create(ctx, cs); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return c.Patch(ctx, cs, client.RawPatch(types.MergePatchType, raw))
}

// ---- apps domain and certificates -------------------------------------------------

func (s *settingsAPI) setApps(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AppsDomain    string `json:"appsDomain"`
		TLS           string `json:"tls"`           // http01 | dns01
		Provider      string `json:"provider"`      // dns01 or manageRecords: hetzner
		ManageRecords bool   `json:"manageRecords"` // keep records for the console and *.<appsDomain>
		Token         string `json:"token"`         // optional; write-only
	}
	if !decode(w, r, &req) {
		return
	}
	apps := normalizeHost(req.AppsDomain)
	token := strings.TrimSpace(req.Token)
	if apps != "" && (!validHostname(apps) || len(apps) > 200) {
		writeFieldError(w, "appsDomain", "Enter a domain, like apps.example.com.")
		return
	}
	tls := kwerftv1.TLSMode(req.TLS)
	switch tls {
	case "":
		tls = kwerftv1.TLSHTTP01
	case kwerftv1.TLSHTTP01, kwerftv1.TLSDNS01:
	default:
		writeFieldError(w, "tls", "Choose HTTP-01 or DNS-01.")
		return
	}
	useDNS := tls == kwerftv1.TLSDNS01 || req.ManageRecords
	if tls == kwerftv1.TLSDNS01 && apps == "" {
		writeFieldError(w, "appsDomain", "A wildcard certificate needs an apps domain.")
		return
	}
	if useDNS {
		if req.Provider == "" {
			req.Provider = "hetzner"
		}
		if req.Provider != "hetzner" {
			writeFieldError(w, "provider", "Only Hetzner DNS is supported.")
			return
		}
	}
	if token != "" && (len(token) > 256 || strings.ContainsAny(token, " \t\r\n")) {
		writeFieldError(w, "token", "That does not look like an API token.")
		return
	}

	c, p, ctx, cancel, err := s.userClient(r)
	defer cancel()
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	cs, err := s.load(ctx, c)
	if err != nil {
		s.kubeError(w, r, p, "settings.apps", apps, "Settings not found.", err)
		return
	}
	view := s.view(cs)
	if apps != "" && (apps == view.ConsoleDomain || apps == view.PendingConsoleDomain) {
		writeFieldError(w, "appsDomain", apps+" is the console's hostname. Apps need a domain of their own, like apps.example.com.")
		return
	}
	if useDNS && token == "" && !view.TokenSet {
		writeFieldError(w, "token", "Enter a Hetzner API token with read and write access to the DNS zone.")
		return
	}

	// Check the token can see the zones before storing anything: the apps
	// domain's for a wildcard certificate (required), and for managed
	// records also the console's (a warning: its record stays manual).
	var warnings []string
	zoneFound := ""
	if token != "" {
		hz := &hetzner.Client{Token: token, Base: s.hetznerAPI, HTTP: s.http}
		hosts := []string{}
		if apps != "" {
			hosts = append(hosts, apps)
		}
		if req.ManageRecords {
			hosts = append(hosts, view.ConsoleDomain)
			if view.PendingConsoleDomain != "" {
				hosts = append(hosts, view.PendingConsoleDomain)
			}
		}
		for i, host := range hosts {
			zone, err := hz.ZoneFor(ctx, host)
			switch {
			case errors.Is(err, hetzner.ErrTokenRejected):
				writeFieldError(w, "token", "Hetzner rejected this token. Create one in the Hetzner Console under Security → API tokens, with read and write access.")
				return
			case errors.Is(err, hetzner.ErrNoZone) && i == 0 && host == apps:
				writeFieldError(w, "appsDomain", "The Hetzner project of this token has no DNS zone for "+apps+". Add the zone in the Hetzner Console (DNS), or use the token of the project that has it.")
				return
			case errors.Is(err, hetzner.ErrNoZone):
				warnings = append(warnings, "The token's project has no zone for "+host+"; create its record by hand.")
			case err != nil:
				warnings = append(warnings, "Could not reach the Hetzner API to check the token ("+err.Error()+"). It is saved anyway.")
			case zoneFound == "":
				zoneFound = zone
			}
			if err != nil && !errors.Is(err, hetzner.ErrNoZone) {
				break
			}
		}
		if cs == nil {
			// The reconciler creates the token's Secret once settings exist.
			if err := s.patchSettings(ctx, c, nil, map[string]any{}); err != nil {
				s.kubeError(w, r, p, "settings.apps", apps, "Settings not found.", err)
				return
			}
			if cs, err = s.load(ctx, c); err != nil {
				s.internalError(w, r, err)
				return
			}
		}
		if err := s.writeToken(ctx, c, token); err != nil {
			if apierrors.IsNotFound(err) {
				writeError(w, http.StatusServiceUnavailable, "The console is still preparing the token's storage. Try again in a moment.")
				return
			}
			s.kubeError(w, r, p, "settings.dns_token", controllers.DNSTokenSecret, "Token storage not found.", err)
			return
		}
		s.audit(r, p.user.Email, "settings.dns_token", "hetzner", "token replaced")
	}

	spec := map[string]any{"appsDomain": nilIfEmpty(apps), "tls": string(tls), "dns": nil}
	if useDNS {
		spec["dns"] = map[string]any{"provider": req.Provider, "manageRecords": req.ManageRecords}
	}
	patch := map[string]any{"spec": spec}
	if token != "" {
		patch["metadata"] = map[string]any{"annotations": map[string]any{
			controllers.AnnotationDNSTokenUpdated: s.now().UTC().Format(time.RFC3339Nano)}}
	}
	if err := s.patchSettings(ctx, c, cs, patch); err != nil {
		s.kubeError(w, r, p, "settings.apps", apps, "Settings not found.", err)
		return
	}
	detail := string(tls)
	if req.ManageRecords {
		detail += ", managed DNS records"
	}
	s.audit(r, p.user.Email, "settings.apps", orNone(apps), detail)
	cs, err = s.load(ctx, c)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	out := map[string]any{"settings": s.view(cs)}
	if len(warnings) > 0 {
		out["warning"] = strings.Join(warnings, " ")
	}
	if zoneFound != "" {
		out["zone"] = zoneFound
	}
	writeJSON(w, http.StatusOK, out)
}

// writeToken patches the token into its Secret as the user. Their role may
// patch this one Secret but not read it, so this is the only way in.
func (s *settingsAPI) writeToken(ctx context.Context, c client.Client, token string) error {
	raw, err := json.Marshal(map[string]any{"data": map[string]string{
		controllers.DNSTokenKey: base64.StdEncoding.EncodeToString([]byte(token)),
	}})
	if err != nil {
		return err
	}
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: controllers.GatewayNamespace, Name: controllers.DNSTokenSecret}}
	// Right after the settings were first created the Secret may not exist yet.
	for deadline := time.Now().Add(5 * time.Second); ; {
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		if !apierrors.IsNotFound(err) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
