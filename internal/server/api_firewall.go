package server

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/firewall"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Server firewall (docs/phase4.md, W3): FirewallRules, and the pending
// change the node agents roll back unless it is confirmed here.
//
// Who decides:
//   - Owners and admins only (the blueprint's permissions); every write is
//     made as the signed-in user (impersonated) and audited. Kubernetes RBAC
//     (roles.yaml: kwerft.dev "*" for owners and admins) is the authority.
//   - Lock-out protection happens here, where the client's address is known:
//     narrowing SSH must keep the address this request comes from, and the
//     address must be one Kwerft can see (not a proxy's). HTTP(S) cannot be
//     narrowed at all (required rules), so the console stays reachable to
//     confirm, roll back or widen again.
//   - Confirming is an annotation on the required rule "ssh"
//     (firewall.AnnotationConfirmed), written as the user; "Apply again"
//     bumps firewall.AnnotationAttempt there. "Roll back now" restores the
//     rules as last confirmed (the controller's snapshot), as the user.
//   - The agents' reports and the desired rules live in two ConfigMaps in
//     kwerft-system that users cannot read; the console reads them with its
//     own identity (SystemReader) to show progress, and never writes them.

type firewallAPI struct {
	*api
	// clientIP is the client's address as the console sees it (clientIP in
	// api.go; tests replace it).
	clientIP func(*http.Request) string
}

func (a *api) registerFirewall(mux *http.ServeMux) {
	fw := &firewallAPI{api: a, clientIP: clientIP}
	if a.cfg.firewallHook != nil {
		a.cfg.firewallHook(fw)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(h, store.RoleOwner, store.RoleAdmin)))
	}
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	mux.HandleFunc("GET /api/v1/firewall", read(fw.get))
	mux.HandleFunc("POST /api/v1/firewall/rules", write(fw.create))
	mux.HandleFunc("PUT /api/v1/firewall/rules/{name}", write(fw.update))
	mux.HandleFunc("DELETE /api/v1/firewall/rules/{name}", write(fw.remove))
	mux.HandleFunc("POST /api/v1/firewall/confirm", write(fw.confirm))
	mux.HandleFunc("POST /api/v1/firewall/rollback", write(fw.rollback))
	mux.HandleFunc("POST /api/v1/firewall/retry", write(fw.retry))
}

// clientAddr is the client's address for lock-out checks, and whether a
// check may trust it. It uses clientIP, which takes X-Real-IP (Traefik sets
// it for proxied traffic); W4 restricts that to requests from Traefik
// (TODO(phase-4) in api.go), and this follows whatever clientIP decides.
func (fw *firewallAPI) clientAddr(r *http.Request) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(fw.clientIP(r))
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	return addr, firewall.Verifiable(addr)
}

// ---- views -----------------------------------------------------------------------------

type firewallRuleJSON struct {
	Name        string   `json:"name"`
	Port        int32    `json:"port"`
	EndPort     int32    `json:"endPort,omitempty"`
	Protocol    string   `json:"protocol"`
	Sources     []string `json:"sources"`
	Nodes       string   `json:"nodes"`
	Description string   `json:"description"`
	Disabled    bool     `json:"disabled"`
	Required    bool     `json:"required"`
	// Editable: "all" (custom rules), "sources" (the SSH rule) or "none".
	Editable string `json:"editable"`
	Ready    bool   `json:"ready"`
	Reason   string `json:"reason,omitempty"`
	Message  string `json:"message,omitempty"`
	// CloudFirewall is the rule's part in the Hetzner Cloud Firewall
	// (FirewallRuleStatus.cloudFirewall); empty without a Cloud API token.
	CloudFirewall string `json:"cloudFirewall,omitempty"`
}

type firewallNodeJSON struct {
	Name         string     `json:"name"`
	ControlPlane bool       `json:"controlPlane"`
	State        string     `json:"state"`
	Message      string     `json:"message,omitempty"`
	UpdatedAt    *time.Time `json:"updatedAt,omitempty"`
}

type firewallPendingJSON struct {
	Revision         string    `json:"revision"`
	Deadline         time.Time `json:"deadline"`
	RemainingSeconds int       `json:"remainingSeconds"`
	Nodes            int       `json:"nodes"`
}

type firewallRolledBackJSON struct {
	Revision string     `json:"revision"`
	At       *time.Time `json:"at,omitempty"`
}

type firewallClientJSON struct {
	IP string `json:"ip"`
	// Verifiable: the address is the client's own, not a proxy's.
	Verifiable bool `json:"verifiable"`
	// SSH: whether this address reaches SSH with the rules as saved.
	SSH bool `json:"ssh"`
}

type firewallJSON struct {
	Rules     []firewallRuleJSON `json:"rules"`
	Revision  string             `json:"revision"`
	Confirmed string             `json:"confirmed"`
	// State: in-sync, pending, rolled-back, failed, applying (agents have
	// not all taken the revision yet), no-agents or unavailable.
	State      string                  `json:"state"`
	Pending    *firewallPendingJSON    `json:"pending,omitempty"`
	RolledBack *firewallRolledBackJSON `json:"rolledBack,omitempty"`
	// CanRollBack: rules as last confirmed exist to go back to.
	CanRollBack bool               `json:"canRollBack"`
	Nodes       []firewallNodeJSON `json:"nodes"`
	Client      firewallClientJSON `json:"client"`
	Problems    []string           `json:"problems,omitempty"`
	// Cloud is the Hetzner Cloud Firewall in front of the Cloud servers
	// (ConsoleSettings status.hetznerCloud.firewall), when a token is set.
	Cloud *kwerftv1.CloudFirewallStatus `json:"cloud,omitempty"`
}

func fwRuleJSON(r *kwerftv1.FirewallRule) firewallRuleJSON {
	out := firewallRuleJSON{
		Name: r.Name, Port: r.Spec.Port, EndPort: r.Spec.EndPort, Protocol: r.Spec.Protocol, Sources: nonNil(r.Spec.Sources),
		Nodes: r.Spec.Nodes, Description: r.Spec.Description, Disabled: r.Spec.Disabled, Required: firewall.IsRequired(r), Editable: "all",
		CloudFirewall: r.Status.CloudFirewall,
	}
	if out.Nodes == "" {
		out.Nodes = "all"
	}
	if out.Required {
		out.Editable = "none"
		if r.Name == firewall.RuleSSH {
			out.Editable = "sources"
		}
	}
	if c := meta.FindStatusCondition(r.Status.Conditions, controllers.ConditionReady); c != nil && c.ObservedGeneration == r.Generation {
		out.Ready, out.Reason, out.Message = c.Status == metav1.ConditionTrue, c.Reason, c.Message
	}
	return out
}

// state is what the console knows about the rules on the nodes.
type firewallState struct {
	desired  firewall.Desired
	snapshot controllers.FirewallSnapshot
	statuses map[string]firewall.NodeStatus
	progress controllers.FirewallProgress
	ok       bool // the ConfigMaps could be read
}

func (fw *firewallAPI) state(ctx context.Context) (firewallState, error) {
	var st firewallState
	if fw.cfg.SystemReader == nil {
		return st, nil
	}
	var cm corev1.ConfigMap
	err := fw.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: firewall.Namespace, Name: firewall.DesiredConfigMap}, &cm)
	if err != nil && !apierrors.IsNotFound(err) {
		return st, err
	}
	_ = json.Unmarshal([]byte(cm.Data[firewall.DesiredKey]), &st.desired)
	_ = json.Unmarshal([]byte(cm.Data[firewall.SnapshotKey]), &st.snapshot)
	if st.statuses, err = controllers.ReadFirewallStatus(ctx, fw.cfg.SystemReader); err != nil {
		return st, err
	}
	st.progress = controllers.AggregateFirewall(st.desired, st.statuses, fw.now())
	st.ok = true
	return st, nil
}

// alwaysSSH are the networks the base chain lets reach SSH before any
// narrowing: the private network (the required rule cluster-private).
func alwaysSSH(rules []kwerftv1.FirewallRule) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range rules {
		if r.Name != firewall.RuleCluster || !firewall.IsRequired(&r) {
			continue
		}
		ps, _ := firewall.ParseSources(r.Spec.Sources)
		for _, p := range ps {
			if p.String() != firewall.PodCIDR {
				out = append(out, p)
			}
		}
	}
	return out
}

func (fw *firewallAPI) get(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	var list kwerftv1.FirewallRuleList
	if err := fw.api.list(ctx, c, &list); err != nil {
		fw.kubeError(w, r, p, "firewall.list", "", "Firewall rules not found.", err)
		return
	}
	st, err := fw.state(ctx)
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	out := firewallJSON{Rules: []firewallRuleJSON{}, Nodes: []firewallNodeJSON{}}
	var sshSources []string
	for i := range list.Items {
		rule := &list.Items[i]
		out.Rules = append(out.Rules, fwRuleJSON(rule))
		if rule.Name == firewall.RuleSSH {
			sshSources = rule.Spec.Sources
		}
	}
	// Required rules first, in the installer's order; then custom ones by name.
	order := map[string]int{}
	for i, req := range firewall.Required("") {
		order[req.Name] = i + 1
	}
	slices.SortFunc(out.Rules, func(a, b firewallRuleJSON) int {
		oa, ob := order[a.Name], order[b.Name]
		if oa == 0 {
			oa = 1000
		}
		if ob == 0 {
			ob = 1000
		}
		if oa != ob {
			return oa - ob
		}
		return strings.Compare(a.Name, b.Name)
	})

	addr, verifiable := fw.clientAddr(r)
	out.Client = firewallClientJSON{IP: fw.clientIP(r), Verifiable: verifiable, SSH: firewall.SSHReaches(sshSources, addr, alwaysSSH(list.Items)...)}
	var cs kwerftv1.ConsoleSettings
	if err := c.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &cs); err == nil &&
		cs.Annotations[controllers.AnnotationHCloudTokenUpdated] != "" && cs.Status.HetznerCloud != nil {
		out.Cloud = cs.Status.HetznerCloud.Firewall
	}

	if !st.ok {
		out.State = "unavailable"
		writeJSON(w, http.StatusOK, out)
		return
	}
	now := fw.now()
	out.Revision, out.Confirmed = st.desired.Revision, st.desired.Confirmed
	out.CanRollBack = st.snapshot.Revision != "" && st.snapshot.Revision != st.desired.Revision
	out.Problems = st.progress.Problems
	names := make([]string, 0, len(st.desired.Nodes))
	for n := range st.desired.Nodes {
		names = append(names, n)
	}
	slices.Sort(names)
	var rolledBackAt *time.Time
	for _, n := range names {
		s, ok := st.statuses[n]
		node := firewallNodeJSON{Name: n, ControlPlane: slices.Contains(st.desired.ControlPlane, n),
			State: controllers.FirewallNodeState(st.desired.Revision, s, ok, now), Message: s.Message}
		if ok {
			at := s.UpdatedAt
			node.UpdatedAt = &at
			if s.RolledBack == st.desired.Revision && s.RolledBackAt != nil && (rolledBackAt == nil || s.RolledBackAt.After(*rolledBackAt)) {
				rolledBackAt = s.RolledBackAt
			}
		}
		out.Nodes = append(out.Nodes, node)
	}
	pr := st.progress
	switch {
	case pr.Nodes == 0 || pr.Reporting == 0:
		out.State = "no-agents"
	case pr.Pending > 0:
		out.State = "pending"
	case pr.RolledBack > 0:
		out.State = "rolled-back"
	case pr.Failed > 0:
		out.State = "failed"
	case pr.InSync == pr.Reporting && pr.Waiting == 0:
		out.State = "in-sync"
	default:
		out.State = "applying"
	}
	if pr.Pending > 0 && pr.Deadline != nil {
		out.Pending = &firewallPendingJSON{Revision: st.desired.Revision, Deadline: *pr.Deadline,
			RemainingSeconds: max(0, int(pr.Deadline.Sub(now).Seconds())), Nodes: pr.Pending}
	}
	if pr.RolledBack > 0 {
		out.RolledBack = &firewallRolledBackJSON{Revision: st.desired.Revision, At: rolledBackAt}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- rules -------------------------------------------------------------------------------

type firewallRuleInput struct {
	Name        string   `json:"name"`
	Port        int32    `json:"port"`
	EndPort     int32    `json:"endPort"`
	Protocol    string   `json:"protocol"`
	Sources     []string `json:"sources"`
	Nodes       string   `json:"nodes"`
	Description string   `json:"description"`
	Disabled    bool     `json:"disabled"`
}

func ruleNotFoundFW(name string) string { return fmt.Sprintf("Firewall rule %q not found.", name) }

// specOf validates a custom rule's input.
func specOf(w http.ResponseWriter, in *firewallRuleInput) (kwerftv1.FirewallRuleSpec, bool) {
	sources, ferr := firewall.NormalizeSources(in.Sources)
	if ferr != nil {
		writeFieldError(w, ferr.Field, ferr.Message)
		return kwerftv1.FirewallRuleSpec{}, false
	}
	spec := kwerftv1.FirewallRuleSpec{
		Port: in.Port, EndPort: in.EndPort, Protocol: strings.ToUpper(strings.TrimSpace(in.Protocol)), Sources: sources,
		Nodes: strings.TrimSpace(in.Nodes), Description: strings.TrimSpace(in.Description), Disabled: in.Disabled,
	}
	if spec.Nodes == "" {
		spec.Nodes = "all"
	}
	if spec.EndPort == spec.Port {
		spec.EndPort = 0
	}
	if len(spec.Description) > 200 {
		writeFieldError(w, "description", "At most 200 characters.")
		return spec, false
	}
	rule := kwerftv1.FirewallRule{Spec: spec}
	if ferr := firewall.Validate(&rule); ferr != nil {
		writeFieldError(w, ferr.Field, ferr.Message)
		return spec, false
	}
	return spec, true
}

func ruleDetailFW(s kwerftv1.FirewallRuleSpec) string {
	ports := strconv.Itoa(int(s.Port))
	if s.EndPort > s.Port {
		ports += "-" + strconv.Itoa(int(s.EndPort))
	}
	from := "anywhere"
	if len(s.Sources) > 0 {
		from = strings.Join(s.Sources, ", ")
	}
	d := fmt.Sprintf("%s %s from %s on %s nodes", s.Protocol, ports, from, cmp.Or(s.Nodes, "all"))
	if s.Disabled {
		d += "; disabled"
	}
	return d
}

func (fw *firewallAPI) create(w http.ResponseWriter, r *http.Request) {
	var in firewallRuleInput
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if !firewall.ValidName(name) {
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63).")
		return
	}
	if firewall.IsRequiredName(name) {
		writeFieldError(w, "name", fmt.Sprintf("%q is the name of a required rule. Choose another.", name))
		return
	}
	spec, ok := specOf(w, &in)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	rule := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := c.Create(ctx, rule); err != nil {
		fw.kubeError(w, r, p, "firewall.rule_create", name, ruleNotFoundFW(name), err)
		return
	}
	fw.audit(r, p.user.Email, "firewall.rule_create", name, ruleDetailFW(spec))
	writeJSON(w, http.StatusCreated, fwRuleJSON(rule))
}

func (fw *firewallAPI) update(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in firewallRuleInput
	if !decode(w, r, &in) {
		return
	}
	if in.Name != "" && in.Name != name {
		writeFieldError(w, "name", "A rule cannot be renamed. Create a new one instead.")
		return
	}
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	var rule kwerftv1.FirewallRule
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &rule); err != nil {
		fw.kubeError(w, r, p, "firewall.rule_update", name, ruleNotFoundFW(name), err)
		return
	}
	var spec kwerftv1.FirewallRuleSpec
	switch {
	case firewall.IsRequired(&rule) && rule.Name != firewall.RuleSSH:
		writeError(w, http.StatusConflict, "This rule is part of the installer's firewall and cannot change.")
		return
	case firewall.IsRequired(&rule):
		// Only the SSH rule's sources; the rest must be what it is.
		if (in.Port != 0 && in.Port != rule.Spec.Port) || (in.Protocol != "" && !strings.EqualFold(in.Protocol, rule.Spec.Protocol)) ||
			(in.EndPort != 0 && in.EndPort != rule.Spec.EndPort) || (in.Nodes != "" && in.Nodes != cmp.Or(rule.Spec.Nodes, "all")) || in.Disabled {
			writeError(w, http.StatusConflict, "Only the SSH rule's sources can change: it stays TCP 22 on every node.")
			return
		}
		sources, ferr := firewall.NormalizeSources(in.Sources)
		if ferr != nil {
			writeFieldError(w, ferr.Field, ferr.Message)
			return
		}
		var list kwerftv1.FirewallRuleList
		if err := fw.api.list(ctx, c, &list); err != nil {
			fw.kubeError(w, r, p, "firewall.rule_update", name, ruleNotFoundFW(name), err)
			return
		}
		if !fw.sshKept(w, r, p, sources, alwaysSSH(list.Items)) {
			return
		}
		spec = rule.Spec
		spec.Sources = sources
	default:
		var ok bool
		if spec, ok = specOf(w, &in); !ok {
			return
		}
	}
	old := rule.Spec
	if err := updateSpec(ctx, c, &rule, func(x *kwerftv1.FirewallRule) bool {
		if !equality.Semantic.DeepEqual(x.Spec, old) {
			return false
		}
		x.Spec = spec
		return true
	}); err != nil {
		fw.kubeError(w, r, p, "firewall.rule_update", name, ruleNotFoundFW(name), err)
		return
	}
	fw.audit(r, p.user.Email, "firewall.rule_update", name, ruleDetailFW(spec))
	writeJSON(w, http.StatusOK, fwRuleJSON(&rule))
}

// sshKept is the lock-out check: narrowed SSH must still let this client in.
func (fw *firewallAPI) sshKept(w http.ResponseWriter, r *http.Request, p *principal, sources []string, always []netip.Prefix) bool {
	if len(sources) == 0 {
		return true
	}
	addr, verifiable := fw.clientAddr(r)
	if !verifiable {
		fw.audit(r, p.user.Email, "firewall.lockout_refused", firewall.RuleSSH, "client address "+fw.clientIP(r)+" cannot be checked")
		writeFieldError(w, "sources", fmt.Sprintf("Kwerft sees your address as %s, which is not where you connect from (a proxy or tunnel hides it), "+
			"so it cannot check that you keep SSH access. Narrow SSH from a browser that reaches the console directly.", fw.clientIP(r)))
		return false
	}
	if !firewall.SSHReaches(sources, addr, always...) {
		bits := 32
		if addr.Is6() {
			bits = 128
		}
		fw.audit(r, p.user.Email, "firewall.lockout_refused", firewall.RuleSSH, "would block SSH from "+addr.String())
		writeFieldError(w, "sources", fmt.Sprintf("This would block SSH from your address %s. Add %s/%d, or a range that contains it.", addr, addr, bits))
		return false
	}
	return true
}

func (fw *firewallAPI) remove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	var rule kwerftv1.FirewallRule
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &rule); err != nil {
		fw.kubeError(w, r, p, "firewall.rule_delete", name, ruleNotFoundFW(name), err)
		return
	}
	if firewall.IsRequired(&rule) {
		writeError(w, http.StatusConflict, "Required rules cannot be deleted.")
		return
	}
	uid := rule.UID
	if err := c.Delete(ctx, &rule, client.Preconditions{UID: &uid}); err != nil {
		fw.kubeError(w, r, p, "firewall.rule_delete", name, ruleNotFoundFW(name), err)
		return
	}
	fw.audit(r, p.user.Email, "firewall.rule_delete", name, ruleDetailFW(rule.Spec))
	w.WriteHeader(http.StatusNoContent)
}

// ---- the pending change ------------------------------------------------------------------

type revisionInput struct {
	Revision string `json:"revision"`
}

// annotateSSH merge-patches an annotation on the required rule "ssh", as the user.
func annotateSSH(ctx context.Context, c client.Client, key, value string) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{key: value}}})
	rule := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: firewall.RuleSSH}}
	return c.Patch(ctx, rule, client.RawPatch(types.MergePatchType, patch))
}

func (fw *firewallAPI) confirm(w http.ResponseWriter, r *http.Request) {
	var in revisionInput
	if !decode(w, r, &in) {
		return
	}
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	st, err := fw.state(ctx)
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	switch {
	case !st.ok:
		writeError(w, http.StatusServiceUnavailable, "The console cannot see the firewall's state right now.")
		return
	case in.Revision == "" || in.Revision != st.desired.Revision:
		writeError(w, http.StatusConflict, "The rules changed meanwhile. Review them and confirm again.")
		return
	case st.progress.RolledBack > 0:
		writeError(w, http.StatusConflict, "Too late: the change was rolled back already. Apply it again to retry.")
		return
	case st.desired.Confirmed == in.Revision:
		writeJSON(w, http.StatusOK, map[string]string{"confirmed": in.Revision})
		return
	case st.progress.Pending == 0:
		writeError(w, http.StatusConflict, "No node is waiting for a confirmation yet. Wait until the change is applied.")
		return
	}
	if err := annotateSSH(ctx, c, firewall.AnnotationConfirmed, in.Revision); err != nil {
		fw.kubeError(w, r, p, "firewall.confirm", firewall.RuleSSH, ruleNotFoundFW(firewall.RuleSSH), err)
		return
	}
	fw.audit(r, p.user.Email, "firewall.confirm", in.Revision, fmt.Sprintf("kept the change on %d nodes", st.progress.Pending))
	writeJSON(w, http.StatusOK, map[string]string{"confirmed": in.Revision})
}

// rollback restores the rules as last confirmed. The agents see their
// confirmed rules again and drop a pending change at once.
func (fw *firewallAPI) rollback(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	st, err := fw.state(ctx)
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	if !st.ok || st.snapshot.Revision == "" {
		writeError(w, http.StatusConflict, "There are no confirmed rules to go back to yet.")
		return
	}
	var list kwerftv1.FirewallRuleList
	if err := c.List(ctx, &list); err != nil {
		fw.kubeError(w, r, p, "firewall.rollback", "", "Firewall rules not found.", err)
		return
	}
	current := map[string]*kwerftv1.FirewallRule{}
	for i := range list.Items {
		current[list.Items[i].Name] = &list.Items[i]
	}
	var changed []string
	for _, e := range st.snapshot.Rules {
		have := current[e.Name]
		delete(current, e.Name)
		switch {
		case have == nil && e.Required:
			continue // the controller recreates it
		case have == nil:
			rule := &kwerftv1.FirewallRule{ObjectMeta: metav1.ObjectMeta{Name: e.Name}, Spec: e.Spec}
			if err := c.Create(ctx, rule); err != nil && !apierrors.IsAlreadyExists(err) {
				fw.kubeError(w, r, p, "firewall.rollback", e.Name, ruleNotFoundFW(e.Name), err)
				return
			}
			changed = append(changed, "created "+e.Name)
		case equality.Semantic.DeepEqual(have.Spec, e.Spec):
		default:
			want := e.Spec
			if firewall.IsRequired(have) {
				want = have.Spec
				want.Sources = e.Spec.Sources // only what the console may change
			}
			if err := updateSpec(ctx, c, have, func(x *kwerftv1.FirewallRule) bool { x.Spec = want; return true }); err != nil {
				fw.kubeError(w, r, p, "firewall.rollback", e.Name, ruleNotFoundFW(e.Name), err)
				return
			}
			changed = append(changed, "restored "+e.Name)
		}
	}
	for name, rule := range current {
		if firewall.IsRequired(rule) {
			continue
		}
		uid := rule.UID
		if err := c.Delete(ctx, rule, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
			fw.kubeError(w, r, p, "firewall.rollback", name, ruleNotFoundFW(name), err)
			return
		}
		changed = append(changed, "deleted "+name)
	}
	slices.Sort(changed)
	fw.audit(r, p.user.Email, "firewall.rollback", st.snapshot.Revision, cmp.Or(strings.Join(changed, ", "), "nothing to restore"))
	writeJSON(w, http.StatusOK, map[string]any{"revision": st.snapshot.Revision, "changed": nonNil(changed)})
}

// retry applies a rolled back (or refused) revision again, as a new attempt
// with a new confirmation window.
func (fw *firewallAPI) retry(w http.ResponseWriter, r *http.Request) {
	var in revisionInput
	if !decode(w, r, &in) {
		return
	}
	c, p, ctx, cancel, err := fw.userClient(r)
	defer cancel()
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	st, err := fw.state(ctx)
	if err != nil {
		fw.internalError(w, r, err)
		return
	}
	switch {
	case !st.ok:
		writeError(w, http.StatusServiceUnavailable, "The console cannot see the firewall's state right now.")
		return
	case in.Revision == "" || in.Revision != st.desired.Revision:
		writeError(w, http.StatusConflict, "The rules changed meanwhile. Review them first.")
		return
	case st.progress.RolledBack == 0 && st.progress.Failed == 0:
		writeError(w, http.StatusConflict, "Nothing was rolled back; there is nothing to apply again.")
		return
	}
	var ssh kwerftv1.FirewallRule
	if err := c.Get(ctx, client.ObjectKey{Name: firewall.RuleSSH}, &ssh); err != nil {
		fw.kubeError(w, r, p, "firewall.retry", firewall.RuleSSH, ruleNotFoundFW(firewall.RuleSSH), err)
		return
	}
	var list kwerftv1.FirewallRuleList
	if err := fw.api.list(ctx, c, &list); err != nil {
		fw.kubeError(w, r, p, "firewall.retry", "", "Firewall rules not found.", err)
		return
	}
	if !fw.sshKept(w, r, p, ssh.Spec.Sources, alwaysSSH(list.Items)) {
		return
	}
	n, _ := strconv.Atoi(ssh.Annotations[firewall.AnnotationAttempt])
	if err := annotateSSH(ctx, c, firewall.AnnotationAttempt, strconv.Itoa(n+1)); err != nil {
		fw.kubeError(w, r, p, "firewall.retry", firewall.RuleSSH, ruleNotFoundFW(firewall.RuleSSH), err)
		return
	}
	fw.audit(r, p.user.Email, "firewall.retry", in.Revision, "applying the rolled back change again")
	writeJSON(w, http.StatusOK, map[string]int{"attempt": n + 1})
}
