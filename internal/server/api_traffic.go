package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hubble"
)

// Traffic API: a project's TrafficRules with Hubble's counts, the
// connections policies dropped, and the project's isolation.
//
//   - Rules are read and written as the signed-in user (impersonation):
//     Kubernetes RBAC decides who manages them (owners and admins
//     everywhere, developers in their projects, viewers read).
//   - Flows come from the console's Hubble aggregator (internal/hubble),
//     which sees the whole cluster, so what a user gets is confined here:
//     a project's flows only after the user listed that project's rules
//     (Kubernetes said yes), the cluster-wide drop list by trafficScope.
//   - Of a flow's other side, users see the namespace and app, never pod
//     names or pod addresses.

const (
	maxDrops       = 50
	maxRuleNameLen = 50 // so "<rule>.traffic-out" and the label stay short
)

func (a *api) registerTraffic(mux *http.ServeMux) {
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }

	mux.HandleFunc("GET /api/v1/projects/{project}/traffic", read(a.trafficOverview))
	mux.HandleFunc("POST /api/v1/projects/{project}/trafficrules", write(a.trafficRuleCreate))
	mux.HandleFunc("PUT /api/v1/projects/{project}/trafficrules/{rule}", write(a.trafficRuleUpdate))
	mux.HandleFunc("DELETE /api/v1/projects/{project}/trafficrules/{rule}", write(a.trafficRuleDelete))
	mux.HandleFunc("PUT /api/v1/projects/{project}/isolation", write(a.projectIsolation))
	mux.HandleFunc("GET /api/v1/traffic/drops", read(a.trafficDrops))
}

// ---- JSON --------------------------------------------------------------------

type trafficRuleJSON struct {
	Name        string                  `json:"name"`
	Description string                  `json:"description,omitempty"`
	Disabled    bool                    `json:"disabled"`
	From        []kwerftv1.TrafficPeer  `json:"from"`
	To          []kwerftv1.TrafficPeer  `json:"to"`
	Ports       []kwerftv1.TrafficPort  `json:"ports"`
	Phase       string                  `json:"phase"` // ready | pending | waiting | failed | disabled
	Reason      string                  `json:"reason,omitempty"`
	Message     string                  `json:"message,omitempty"`
	Policies    []string                `json:"policies"`
	Counts      *kwerftv1.TrafficCounts `json:"counts,omitempty"`
	Generation  int64                   `json:"generation"`
	Created     time.Time               `json:"created"`
}

type dropJSON struct {
	From       hubble.Side        `json:"from"`
	To         hubble.Side        `json:"to"`
	Port       uint32             `json:"port,omitempty"`
	Protocol   string             `json:"protocol,omitempty"`
	Egress     bool               `json:"egress"`
	Count      int64              `json:"count"`
	First      time.Time          `json:"first"`
	Last       time.Time          `json:"last"`
	Suggestion *hubble.Suggestion `json:"suggestion,omitempty"`
}

type hubbleJSON struct {
	hubble.Status
	Window string `json:"window"`
}

type trafficJSON struct {
	Project  string            `json:"project"`
	Isolated bool              `json:"isolated"`
	Hubble   hubbleJSON        `json:"hubble"`
	Rules    []trafficRuleJSON `json:"rules"`
	Drops    []dropJSON        `json:"drops"`
}

func (a *api) hubbleStatus() hubbleJSON {
	if a.cfg.Hubble == nil {
		return hubbleJSON{Status: hubble.Status{State: "off",
			Message: "Hubble is off on this cluster (installed with --lite), so there are no counts or dropped connections."}, Window: "1h"}
	}
	return hubbleJSON{Status: a.cfg.Hubble.Status(), Window: "1h"}
}

func trafficRuleSummary(tr *kwerftv1.TrafficRule, live *hubble.Aggregator) trafficRuleJSON {
	out := trafficRuleJSON{
		Name: tr.Name, Description: tr.Spec.Description, Disabled: tr.Spec.Disabled,
		From: tr.Spec.From, To: tr.Spec.To, Ports: tr.Spec.Ports, Policies: tr.Status.Policies,
		Counts: tr.Status.Counts, Generation: tr.Generation, Created: tr.CreationTimestamp.UTC(),
	}
	if out.Ports == nil {
		out.Ports = []kwerftv1.TrafficPort{}
	}
	if out.Policies == nil {
		out.Policies = []string{}
	}
	if live != nil && !tr.Spec.Disabled {
		if c := live.RuleCounts(tr); c != nil {
			out.Counts = c
		}
	}
	c := meta.FindStatusCondition(tr.Status.Conditions, controllers.ConditionReady)
	switch {
	case c == nil || c.ObservedGeneration < tr.Generation:
		out.Phase = "pending"
	case c.Reason == "Disabled":
		out.Phase = "disabled"
	case c.Status == metav1.ConditionTrue:
		out.Phase = "ready"
	case c.Reason == "AppNotFound" || c.Reason == "AwaitingPeer":
		out.Phase = "waiting"
	default:
		out.Phase = "failed"
	}
	if c != nil {
		out.Reason, out.Message = c.Reason, c.Message
	}
	return out
}

// dropsJSON turns drops into what users see. Suggestions are offered only
// for projects in scope (the user may at least see them; RBAC decides on
// the create).
func dropsJSON(drops []hubble.Drop, scope hubble.Scope) []dropJSON {
	out := make([]dropJSON, 0, len(drops))
	for _, d := range drops {
		j := dropJSON{From: d.From, To: d.To, Port: d.Port, Protocol: d.Protocol, Egress: d.Egress,
			Count: d.Count, First: d.First.UTC(), Last: d.Last.UTC()}
		if s := hubble.Suggest(d.DropKey); s != nil && scope.Has(s.Project) {
			j.Suggestion = s
		}
		out = append(out, j)
	}
	return out
}

// ---- handlers ----------------------------------------------------------------

func ruleTarget(project, name string) string { return project + "/" + name }

func trafficRuleNotFound(project, name string) string {
	return fmt.Sprintf("Traffic rule %q not found in project %q.", name, project)
}

// trafficOverview is the Network › Traffic rules tab for one project.
func (a *api) trafficOverview(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	var proj kwerftv1.Project
	if err := c.Get(ctx, types.NamespacedName{Name: project}, &proj); err != nil {
		a.kubeError(w, r, p, "traffic.read", project, fmt.Sprintf("Project %q not found.", project), err)
		return
	}
	// Listing the rules as the user is the access check for the flows too.
	var rules kwerftv1.TrafficRuleList
	if err := c.List(ctx, &rules, client.InNamespace(project)); err != nil {
		a.kubeError(w, r, p, "traffic.read", project, fmt.Sprintf("Project %q not found.", project), err)
		return
	}
	out := trafficJSON{
		Project: project, Isolated: proj.Spec.Isolated == nil || *proj.Spec.Isolated,
		Hubble: a.hubbleStatus(), Rules: make([]trafficRuleJSON, 0, len(rules.Items)), Drops: []dropJSON{},
	}
	for i := range rules.Items {
		out.Rules = append(out.Rules, trafficRuleSummary(&rules.Items[i], a.cfg.Hubble))
	}
	slices.SortFunc(out.Rules, func(x, y trafficRuleJSON) int { return strings.Compare(x.Name, y.Name) })
	if a.cfg.Hubble != nil {
		scope := hubble.Namespaces(project)
		out.Drops = dropsJSON(a.cfg.Hubble.Drops(scope, nil, maxDrops), scope)
	}
	writeJSON(w, http.StatusOK, out)
}

// trafficScope is what the cluster-wide drop list may show: everything for
// owners and admins, otherwise the namespaces of the projects the user can
// list. TODO(phase-4 W1): use projectScope.
func (a *api) trafficScope(ctx context.Context, c client.Client, p *principal) (hubble.Scope, error) {
	if unconfined(p) {
		return hubble.Unconfined(), nil
	}
	var projects kwerftv1.ProjectList
	if err := a.list(ctx, c, &projects); err != nil {
		return hubble.Scope{}, err
	}
	names := make([]string, 0, len(projects.Items))
	for _, pr := range projects.Items {
		names = append(names, pr.Name)
	}
	return hubble.Namespaces(names...), nil
}

// trafficDrops lists dropped connections across the user's projects (the
// Overview's "needs attention").
func (a *api) trafficDrops(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	scope, err := a.trafficScope(ctx, c, p)
	if err != nil {
		a.kubeError(w, r, p, "traffic.read", "", "No projects found.", err)
		return
	}
	out := struct {
		Hubble hubbleJSON `json:"hubble"`
		Drops  []dropJSON `json:"drops"`
	}{Hubble: a.hubbleStatus(), Drops: []dropJSON{}}
	if a.cfg.Hubble != nil {
		out.Drops = dropsJSON(a.cfg.Hubble.Drops(scope, nil, maxDrops), scope)
	}
	writeJSON(w, http.StatusOK, out)
}

type trafficRuleRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Disabled    bool                   `json:"disabled"`
	From        []kwerftv1.TrafficPeer `json:"from"`
	To          []kwerftv1.TrafficPeer `json:"to"`
	Ports       []kwerftv1.TrafficPort `json:"ports"`
	// Generation of the rule that was edited (updates); 0 skips the check.
	Generation int64 `json:"generation"`
}

func (req *trafficRuleRequest) spec() kwerftv1.TrafficRuleSpec {
	return kwerftv1.TrafficRuleSpec{From: req.From, To: req.To, Ports: req.Ports,
		Description: strings.TrimSpace(req.Description), Disabled: req.Disabled}
}

// validTrafficRule answers 422 naming the field (without "spec.") for a rule
// the reconciler would refuse.
func validTrafficRule(w http.ResponseWriter, project string, spec *kwerftv1.TrafficRuleSpec) bool {
	if len(spec.Description) > 256 {
		invalid(w, "description", "Keep the description under 256 characters.")
		return false
	}
	if len(spec.From) > 32 || len(spec.To) > 32 || len(spec.Ports) > 32 {
		invalid(w, "", "A rule has at most 32 sources, destinations and ports each.")
		return false
	}
	var fe *controllers.TrafficFieldError
	if err := controllers.ValidateTrafficRule(project, spec); errors.As(err, &fe) {
		invalid(w, strings.TrimPrefix(fe.Field, "spec."), fe.Message)
		return false
	} else if err != nil {
		invalid(w, "", err.Error())
		return false
	}
	return true
}

func (a *api) trafficRuleCreate(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req trafficRuleRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(validation.IsDNS1123Label(req.Name)) > 0 || len(req.Name) > maxRuleNameLen {
		invalid(w, "name", fmt.Sprintf("Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most %d).", maxRuleNameLen))
		return
	}
	spec := req.spec()
	if !validTrafficRule(w, project, &spec) {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	tr := &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Name: req.Name, Namespace: project}, Spec: spec}
	target := ruleTarget(project, req.Name)
	if err := c.Create(ctx, tr); err != nil {
		a.kubeError(w, r, p, "trafficrule.create", target, trafficRuleNotFound(project, req.Name), err)
		return
	}
	a.audit(r, p.user.Email, "trafficrule.create", target, controllers.DescribeTrafficRule(project, &spec))
	writeJSON(w, http.StatusCreated, trafficRuleSummary(tr, nil))
}

func (a *api) trafficRuleUpdate(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("rule")
	var req trafficRuleRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Name != "" && req.Name != name {
		invalid(w, "name", "A rule cannot be renamed; create a new one and delete this one.")
		return
	}
	spec := req.spec()
	if !validTrafficRule(w, project, &spec) {
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := ruleTarget(project, name)
	var tr kwerftv1.TrafficRule
	if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: name}, &tr); err != nil {
		a.kubeError(w, r, p, "trafficrule.update", target, trafficRuleNotFound(project, name), err)
		return
	}
	if req.Generation != 0 && req.Generation != tr.Generation {
		writeError(w, http.StatusConflict, "Someone else changed this rule in the meantime. Reload and try again.")
		return
	}
	tr.Spec = spec
	if err := c.Update(ctx, &tr); err != nil {
		a.kubeError(w, r, p, "trafficrule.update", target, trafficRuleNotFound(project, name), err)
		return
	}
	detail := controllers.DescribeTrafficRule(project, &spec)
	if spec.Disabled {
		detail = "disabled: " + detail
	}
	a.audit(r, p.user.Email, "trafficrule.update", target, detail)
	writeJSON(w, http.StatusOK, trafficRuleSummary(&tr, nil))
}

func (a *api) trafficRuleDelete(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("rule")
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	target := ruleTarget(project, name)
	if err := c.Delete(ctx, &kwerftv1.TrafficRule{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project}}); err != nil {
		a.kubeError(w, r, p, "trafficrule.delete", target, trafficRuleNotFound(project, name), err)
		return
	}
	a.audit(r, p.user.Email, "trafficrule.delete", target, "")
	w.WriteHeader(http.StatusNoContent)
}

// projectIsolation turns a project's isolation on or off (owners and admins:
// Projects are theirs to change).
func (a *api) projectIsolation(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("project")
	var req struct {
		Isolated *bool `json:"isolated"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Isolated == nil {
		invalid(w, "isolated", "Say whether the project is isolated (true or false).")
		return
	}
	c, p, ctx, cancel, err := a.userClient(r)
	defer cancel()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	patch := fmt.Appendf(nil, `{"spec":{"isolated":%t}}`, *req.Isolated)
	proj := &kwerftv1.Project{ObjectMeta: metav1.ObjectMeta{Name: project}}
	if err := c.Patch(ctx, proj, client.RawPatch(types.MergePatchType, patch)); err != nil {
		a.kubeError(w, r, p, "project.isolation", project, fmt.Sprintf("Project %q not found.", project), err)
		return
	}
	detail := "isolated"
	if !*req.Isolated {
		detail = "open to other projects"
	}
	a.audit(r, p.user.Email, "project.isolation", project, detail)
	writeJSON(w, http.StatusOK, map[string]any{"project": project, "isolated": *req.Isolated})
}
