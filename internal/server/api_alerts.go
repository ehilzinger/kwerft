package server

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/alerting"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/observability"
	"github.com/ehilzinger/kwerft/internal/store"
)

// Alerts, silences, alert rules and notification channels (docs/phase3.md).
//
// Who decides:
//   - Alerts and silences live in Alertmanager, which has no users. The
//     console reads and writes them with its own access (in-cluster) and
//     confines them itself, like metrics and logs: alerts carrying the
//     namespace of a project the user reaches (scope.go) are that project's;
//     all others (nodes, the console's certificate, platform namespaces,
//     projects the user does not reach) are not shown, except to owners and
//     admins. Developers silence alerts of their projects (a silence must
//     match namespace=<project> exactly; in a Members project the role given
//     there counts); owners and admins any alert. Viewers only look. Every
//     change is audited.
//   - Rules are cluster-wide. Developers and viewers see the rules that
//     watch only projects they reach (or every project, naming none), and
//     developers write only rules watching projects they work in.
//   - AlertRules and NotificationChannels are written as the signed-in user
//     (impersonated), so Kubernetes RBAC decides (roles.yaml): owners and
//     admins write both, developers write rules. RBAC cannot see a rule's
//     condition, so the console keeps Custom rules (spec.expr, arbitrary
//     MetricsQL over every metric) to owners and admins: developers may not
//     create one, turn a rule into one, or change or delete one.
//   - Channel credentials are write-only, as for Git connections: the
//     console merge-patches the Secret notify-<channel> in
//     kwerft-observability as the user (the reconciler's Role grants owners
//     and admins patch on exactly those Secrets), no endpoint returns them
//     and the audit log only says that they changed. The console reads them
//     with its own identity for one thing: a test send an owner or admin
//     asked for, after reading the channel as that user.

type alertsAPI struct {
	*api
	am       *alerting.Alertmanager
	metrics  *alerting.Metrics
	notifier *alerting.Notifier
	// credentialsWait is how long a write waits for the reconciler to
	// create a new channel's Secret and grant access to it.
	credentialsWait time.Duration
}

func (a *api) registerAlerts(mux *http.ServeMux) {
	al := &alertsAPI{
		api: a, am: &alerting.Alertmanager{}, metrics: &alerting.Metrics{},
		notifier: &alerting.Notifier{Now: a.now}, credentialsWait: 15 * time.Second,
	}
	if a.cfg.alertsHook != nil {
		a.cfg.alertsHook(al)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.requireUser(a.requireKube(h)) }
	write := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	may := func(perm string, h http.HandlerFunc) http.HandlerFunc {
		return write(a.requireRole(h, access.RolesWith(perm)...))
	}
	admin := func(h http.HandlerFunc) http.HandlerFunc {
		return write(a.requireRole(h, store.RoleOwner, store.RoleAdmin))
	}

	mux.HandleFunc("GET /api/v1/alerts", read(al.list))
	mux.HandleFunc("GET /api/v1/alerts/silences", read(al.silences))
	// Everyone who sees an alert may ask; maySilence decides by the role in
	// the alert's project (a console viewer can be a developer in a Members
	// project).
	mux.HandleFunc("POST /api/v1/alerts/silences", write(al.silenceCreate))
	mux.HandleFunc("DELETE /api/v1/alerts/silences/{id}", write(al.silenceDelete))

	mux.HandleFunc("GET /api/v1/alerts/conditions", read(al.conditions))
	mux.HandleFunc("GET /api/v1/alerts/rules", read(al.rules))
	mux.HandleFunc("POST /api/v1/alerts/rules", may(access.AlertRules, al.ruleCreate))
	mux.HandleFunc("PUT /api/v1/alerts/rules/{name}", may(access.AlertRules, al.ruleUpdate))
	mux.HandleFunc("DELETE /api/v1/alerts/rules/{name}", may(access.AlertRules, al.ruleDelete))

	mux.HandleFunc("GET /api/v1/alerts/channels", read(al.channels))
	mux.HandleFunc("POST /api/v1/alerts/channels", admin(al.channelCreate))
	mux.HandleFunc("PUT /api/v1/alerts/channels/{name}", admin(al.channelUpdate))
	mux.HandleFunc("DELETE /api/v1/alerts/channels/{name}", admin(al.channelDelete))
	mux.HandleFunc("POST /api/v1/alerts/channels/{name}/test", admin(al.channelTest))
}

const alertmanagerDown = "Alertmanager is not reachable. Alerts still fire and notify; the console cannot show them right now."

func (al *alertsAPI) upstreamError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, alerting.ErrUnavailable) {
		al.cfg.Logger.Warn("alerting backend unavailable", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, alertmanagerDown)
		return
	}
	al.internalError(w, r, err)
}

// ---- who sees what -------------------------------------------------------------------

// visibility is what a user may see of alerts: the namespaces of the
// projects they reach (scope.go), and whether platform alerts too.
type visibility struct {
	projects map[string]bool
	platform bool
	scope    *projectScope
}

func (al *alertsAPI) visibility(ctx context.Context, _ client.Client, p *principal) (visibility, error) {
	scope, err := al.projectScope(ctx, p)
	if err != nil {
		return visibility{}, err
	}
	v := visibility{projects: map[string]bool{}, platform: scope.platform, scope: scope}
	for _, ns := range scope.namespaces() {
		v.projects[ns] = true
	}
	return v, nil
}

// maySilence: owners and admins silence anything they see; others the
// alerts of a project where their role (in a Members project, the one given
// there) may silence.
func (v visibility) maySilence(project string) bool {
	return v.platform || (project != "" && access.Allowed(v.scope.role(project), access.SilenceAlerts))
}

// ruleProjects are the projects an alert rule watches; nil means all of them
// (no scope), including any created later.
func ruleProjects(spec *kwerftv1.AlertRuleSpec) []string {
	if len(spec.Scope.Projects) == 0 && len(spec.Scope.Apps) == 0 {
		return nil
	}
	out := slices.Clone(spec.Scope.Projects)
	for _, a := range spec.Scope.Apps {
		project, _, _ := strings.Cut(a, "/")
		out = append(out, project)
	}
	return out
}

// seesRule: a rule watching only projects the user reaches, or everything
// when the user reaches every project. Rules about other projects are not
// shown: their scope names those projects and apps.
func (v visibility) seesRule(spec *kwerftv1.AlertRuleSpec) bool {
	if v.scope.reachesAll() {
		return true // nothing to hide (a project that does not exist yet included)
	}
	projects := ruleProjects(spec)
	if projects == nil {
		return true // "every project" names none
	}
	for _, p := range projects {
		if !v.scope.reaches(p) {
			return false
		}
	}
	return true
}

// mayWriteRule: owners and admins any rule; others only rules that watch
// nothing but projects where their role may write rules, so a rule cannot be
// pointed at (or notify from) a project they do not reach. A rule without a
// scope watches every project, so only someone who reaches all of them may
// write one.
func (v visibility) mayWriteRule(spec *kwerftv1.AlertRuleSpec) bool {
	if v.platform {
		return true
	}
	projects := ruleProjects(spec)
	if projects == nil {
		if !v.scope.reachesAll() {
			return false
		}
		projects = slices.Collect(maps.Keys(v.scope.roles))
	}
	for _, p := range projects {
		role := v.scope.role(p)
		if role == "" && v.scope.reachesAll() {
			// No such project (yet): someone who reaches every project
			// may name one, as they may write a rule for all of them.
			continue
		}
		if !access.Allowed(role, access.AlertRules) {
			return false
		}
	}
	return true
}

// project is the project an alert belongs to ("" for platform alerts).
func (v visibility) project(labels map[string]string) string {
	if ns := labels["namespace"]; ns != "" && v.projects[ns] {
		return ns
	}
	return ""
}

func (v visibility) sees(labels map[string]string) bool {
	return v.platform || v.project(labels) != ""
}

// silenceProject is the project a silence is confined to: an exact
// namespace matcher naming a project the user reads.
func (v visibility) silenceProject(ms []alerting.Matcher) string {
	for _, m := range ms {
		if m.Name == "namespace" && m.Equal() && v.projects[m.Value] {
			return m.Value
		}
	}
	return ""
}

func (v visibility) seesSilence(ms []alerting.Matcher) bool {
	return v.platform || v.silenceProject(ms) != ""
}

// ---- alerts ----------------------------------------------------------------------------

type alertJSON struct {
	Fingerprint   string            `json:"fingerprint"`
	Rule          string            `json:"rule"`
	Severity      string            `json:"severity"`
	State         string            `json:"state"` // firing | silenced | resolved
	Summary       string            `json:"summary"`
	Description   string            `json:"description"`
	Project       string            `json:"project,omitempty"`
	App           string            `json:"app,omitempty"`
	Labels        map[string]string `json:"labels"`
	StartsAt      time.Time         `json:"startsAt"`
	EndsAt        *time.Time        `json:"endsAt,omitempty"`
	SilencedUntil *time.Time        `json:"silencedUntil,omitempty"`
	SilencedBy    []string          `json:"silencedBy"`
	ConsoleURL    string            `json:"consoleURL,omitempty"`
}

func newAlertJSON(v visibility, labels map[string]string) alertJSON {
	out := alertJSON{
		Rule: labels[observability.LabelRule], Severity: labels["severity"], Labels: labels,
		Project: v.project(labels), SilencedBy: []string{},
	}
	if out.Project != "" {
		out.App = labels["app"]
	}
	return out
}

var severityOrder = map[string]int{"critical": 0, "warning": 1, "info": 2}

func sortAlerts(out []alertJSON) {
	slices.SortStableFunc(out, func(a, b alertJSON) int {
		if c := cmp.Compare(severityOrder[a.Severity], severityOrder[b.Severity]); c != 0 && a.State != "resolved" {
			return c
		}
		if a.State == "resolved" {
			return b.EndsAt.Compare(*a.EndsAt)
		}
		return b.StartsAt.Compare(a.StartsAt)
	})
}

func (al *alertsAPI) list(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state != "" && state != "firing" && state != "silenced" && state != "resolved" {
		writeError(w, http.StatusBadRequest, "state must be firing, silenced or resolved.")
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	v, err := al.visibility(ctx, c, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.list", "", "Projects not found.", err)
		return
	}
	current, err := al.am.Alerts(ctx)
	if err != nil {
		al.upstreamError(w, r, err)
		return
	}
	out := []alertJSON{}
	if state == "resolved" {
		resolved, err := al.resolved(ctx, v, current)
		if err != nil {
			al.upstreamError(w, r, err)
			return
		}
		out = resolved
	} else {
		var silences map[string]alerting.Silence
		for _, a := range current {
			if !v.sees(a.Labels) {
				continue
			}
			j := newAlertJSON(v, a.Labels)
			j.Fingerprint, j.StartsAt = a.Fingerprint, a.StartsAt
			j.Summary, j.Description, j.ConsoleURL = a.Annotations["summary"], a.Annotations["description"], a.Annotations["console_url"]
			j.State = "firing"
			if len(a.Status.SilencedBy) > 0 {
				j.State = "silenced"
				j.SilencedBy = a.Status.SilencedBy
				if silences == nil {
					silences = map[string]alerting.Silence{}
					all, err := al.am.Silences(ctx)
					if err != nil {
						al.upstreamError(w, r, err)
						return
					}
					for _, s := range all {
						silences[s.ID] = s
					}
				}
				for _, id := range a.Status.SilencedBy {
					if s, ok := silences[id]; ok && (j.SilencedUntil == nil || s.EndsAt.After(*j.SilencedUntil)) {
						until := s.EndsAt
						j.SilencedUntil = &until
					}
				}
			}
			if state == "" || state == j.State {
				out = append(out, j)
			}
		}
	}
	sortAlerts(out)
	writeJSON(w, http.StatusOK, out)
}

// resolvedAfter: vmalert writes ALERTS at every evaluation (20s, or 10s)
// while an alert fires, so a series silent this long has stopped.
const resolvedAfter = 90 * time.Second

// resolved are the alerts that fired in the last 24 hours and no longer do:
// Alertmanager forgets them, but vmalert writes the ALERTS series.
func (al *alertsAPI) resolved(ctx context.Context, v visibility, current []alerting.AMAlert) ([]alertJSON, error) {
	const sel = `ALERTS{alertstate="firing",` + observability.LabelRule + `!=""}[24h]`
	now := al.now()
	last, err := al.metrics.Query(ctx, "tlast_over_time("+sel+")", now)
	if err != nil {
		return nil, err
	}
	first, err := al.metrics.Query(ctx, "tfirst_over_time("+sel+")", now)
	if err != nil {
		return nil, err
	}
	key := func(m map[string]string) map[string]string {
		out := make(map[string]string, len(m))
		for k, val := range m {
			if k != "__name__" && k != "alertstate" {
				out[k] = val
			}
		}
		return out
	}
	active := map[string]bool{}
	for _, a := range current {
		active[alerting.Fingerprint(a.Labels)] = true
	}
	starts := map[string]time.Time{}
	for _, s := range first {
		starts[alerting.Fingerprint(key(s.Metric))] = time.Unix(int64(s.Value), 0).UTC()
	}
	host := al.consoleDomain()
	out := []alertJSON{}
	for _, s := range last {
		labels := key(s.Metric)
		fp := alerting.Fingerprint(labels)
		if active[fp] || !v.sees(labels) {
			continue
		}
		ended := time.Unix(int64(s.Value), 0).UTC()
		if now.Sub(ended) < resolvedAfter {
			// Still written at the last evaluations: firing, even if its
			// labels differ from Alertmanager's copy (vmalert may add some).
			continue
		}
		j := newAlertJSON(v, labels)
		j.Fingerprint, j.State = fp, "resolved"
		j.EndsAt = &ended
		j.StartsAt = starts[fp]
		if j.StartsAt.IsZero() {
			j.StartsAt = ended
		}
		j.Summary = j.Rule + ": " + alertTarget(labels)
		j.ConsoleURL = alertLink(labels, host)
		out = append(out, j)
	}
	sortAlerts(out)
	if len(out) > 200 {
		out = out[:200]
	}
	return out, nil
}

// alertTarget names what an alert is about, from its labels.
func alertTarget(l map[string]string) string {
	switch {
	case l["namespace"] != "" && l["app"] != "":
		return l["namespace"] + "/" + l["app"]
	case l["namespace"] != "" && l["schedule"] != "":
		return l["namespace"] + "/" + l["schedule"]
	case l["namespace"] != "" && l["persistentvolumeclaim"] != "":
		return l["namespace"] + "/" + l["persistentvolumeclaim"]
	case l["namespace"] != "" && l["pod"] != "":
		return l["namespace"] + "/" + l["pod"]
	case l["hostname"] != "":
		return l["hostname"]
	case l["node"] != "":
		return l["node"]
	case l["namespace"] != "":
		return l["namespace"]
	}
	return "the cluster"
}

// alertLink is the console page for an alert, like the console_url
// annotation (internal/alerting) for alerts Alertmanager no longer has.
func alertLink(l map[string]string, host string) string {
	if host == "" {
		return ""
	}
	base := "https://" + host
	ns := url.PathEscape(l["namespace"])
	switch {
	case l["namespace"] != "" && l["app"] != "":
		return base + "/apps/" + ns + "/" + url.PathEscape(l["app"]) + "?tab=logs"
	case l["namespace"] != "" && l["schedule"] != "":
		return base + "/jobs/" + ns + "/schedules/" + url.PathEscape(l["schedule"])
	case l["namespace"] != "" && l["persistentvolumeclaim"] != "":
		return base + "/apps/volumes?project=" + url.QueryEscape(l["namespace"])
	}
	return base + "/monitoring"
}

// ---- silences ----------------------------------------------------------------------------

type silenceJSON struct {
	ID        string             `json:"id"`
	Matchers  []alerting.Matcher `json:"matchers"`
	StartsAt  time.Time          `json:"startsAt"`
	EndsAt    time.Time          `json:"endsAt"`
	CreatedBy string             `json:"createdBy"`
	Comment   string             `json:"comment"`
	State     string             `json:"state"`
	Project   string             `json:"project,omitempty"`
}

func silenceView(v visibility, s alerting.Silence) silenceJSON {
	out := silenceJSON{ID: s.ID, Matchers: s.Matchers, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
		CreatedBy: s.CreatedBy, Comment: s.Comment, Project: v.silenceProject(s.Matchers)}
	if s.Status != nil {
		out.State = s.Status.State
	}
	if out.Matchers == nil {
		out.Matchers = []alerting.Matcher{}
	}
	return out
}

func (al *alertsAPI) silences(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	v, err := al.visibility(ctx, c, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.silences", "", "Projects not found.", err)
		return
	}
	all, err := al.am.Silences(ctx)
	if err != nil {
		al.upstreamError(w, r, err)
		return
	}
	out := []silenceJSON{}
	for _, s := range all {
		if s.Status != nil && s.Status.State == "expired" || !v.seesSilence(s.Matchers) {
			continue
		}
		out = append(out, silenceView(v, s))
	}
	slices.SortFunc(out, func(a, b silenceJSON) int { return a.EndsAt.Compare(b.EndsAt) })
	writeJSON(w, http.StatusOK, out)
}

type silenceInput struct {
	Fingerprint string             `json:"fingerprint"`
	Matchers    []alerting.Matcher `json:"matchers"`
	Duration    string             `json:"duration"`
	Comment     string             `json:"comment"`
}

var (
	labelName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	silenceID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)
)

const maxSilence = 30 * 24 * time.Hour

func (al *alertsAPI) silenceCreate(w http.ResponseWriter, r *http.Request) {
	var in silenceInput
	if !decode(w, r, &in) {
		return
	}
	d, err := alerting.ParseDuration(in.Duration)
	if err != nil || d < time.Minute || d > maxSilence {
		writeFieldError(w, "duration", "Silence for 1 minute up to 30 days, like 1h or 24h.")
		return
	}
	comment := strings.TrimSpace(in.Comment)
	if comment == "" {
		comment = "Silenced from the Kwerft console"
	}
	if len(comment) > 1000 {
		writeFieldError(w, "comment", "Keep the comment under 1000 characters.")
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	v, err := al.visibility(ctx, c, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.silence_create", "", "Projects not found.", err)
		return
	}
	var matchers []alerting.Matcher
	switch {
	case in.Fingerprint != "" && len(in.Matchers) > 0:
		writeFieldError(w, "matchers", "Silence either one alert (fingerprint) or by matchers, not both.")
		return
	case in.Fingerprint != "":
		current, err := al.am.Alerts(ctx)
		if err != nil {
			al.upstreamError(w, r, err)
			return
		}
		idx := slices.IndexFunc(current, func(a alerting.AMAlert) bool { return a.Fingerprint == in.Fingerprint })
		if idx < 0 || !v.sees(current[idx].Labels) {
			writeError(w, http.StatusNotFound, "Alert not found. It may have resolved meanwhile.")
			return
		}
		// Exactly this alert: every label, as Alertmanager's own UI does.
		for k, val := range current[idx].Labels {
			matchers = append(matchers, alerting.Matcher{Name: k, Value: val, IsEqual: boolPtr(true)})
		}
		slices.SortFunc(matchers, func(a, b alerting.Matcher) int { return strings.Compare(a.Name, b.Name) })
	case len(in.Matchers) > 0:
		equal := 0
		for _, m := range in.Matchers {
			if !labelName.MatchString(m.Name) || len(m.Value) > 1024 || len(in.Matchers) > 20 {
				writeFieldError(w, "matchers", "Matchers need a label name (letters, digits, _) and a value.")
				return
			}
			if m.Equal() && m.Value != "" {
				equal++
			}
			matchers = append(matchers, alerting.Matcher{Name: m.Name, Value: m.Value, IsRegex: m.IsRegex, IsEqual: boolPtr(m.IsEqual == nil || *m.IsEqual)})
		}
		if equal == 0 {
			writeFieldError(w, "matchers", "Add at least one exact matcher, so the silence cannot mute every alert.")
			return
		}
	default:
		writeFieldError(w, "fingerprint", "Choose the alert to silence.")
		return
	}
	if !v.platform && v.silenceProject(matchers) == "" {
		al.audit(r, p.user.Email, "alerts.silence_create.denied", "", "not confined to a project")
		writeError(w, http.StatusForbidden, "Your role may silence alerts of projects only.")
		return
	}
	if project := v.silenceProject(matchers); !v.maySilence(project) {
		al.audit(r, p.user.Email, "alerts.silence_create.denied", project, "role in the project may not silence")
		writeError(w, http.StatusForbidden, "Your role in this project does not allow silencing alerts.")
		return
	}
	now := al.now()
	s := alerting.Silence{Matchers: matchers, StartsAt: now, EndsAt: now.Add(d), CreatedBy: p.user.Email, Comment: comment}
	id, err := al.am.CreateSilence(ctx, s)
	if err != nil {
		al.upstreamError(w, r, err)
		return
	}
	s.ID = id
	s.Status = &struct {
		State string `json:"state"`
	}{State: "active"}
	al.audit(r, p.user.Email, "alerts.silence_create", id, matchersString(matchers)+" for "+d.String())
	writeJSON(w, http.StatusCreated, silenceView(v, s))
}

func matchersString(ms []alerting.Matcher) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		op := "="
		switch {
		case m.IsRegex && m.Equal():
			op = "=~"
		case m.IsRegex:
			op = "!~"
		case !m.Equal():
			op = "!="
		}
		parts = append(parts, m.Name+op+strconv.Quote(m.Value))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func boolPtr(b bool) *bool { return &b }

func (al *alertsAPI) silenceDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !silenceID.MatchString(id) {
		writeError(w, http.StatusNotFound, "Silence not found.")
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	v, err := al.visibility(ctx, c, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.silence_delete", id, "Projects not found.", err)
		return
	}
	s, err := al.am.Silence(ctx, id)
	if errors.Is(err, alerting.ErrNotFound) || err == nil && !v.seesSilence(s.Matchers) {
		writeError(w, http.StatusNotFound, "Silence not found.")
		return
	}
	if err != nil {
		al.upstreamError(w, r, err)
		return
	}
	if project := v.silenceProject(s.Matchers); !v.maySilence(project) {
		al.audit(r, p.user.Email, "alerts.silence_delete.denied", id, "role in the project may not silence")
		writeError(w, http.StatusForbidden, "Your role in this project does not allow silencing alerts.")
		return
	}
	if err := al.am.DeleteSilence(ctx, id); err != nil && !errors.Is(err, alerting.ErrNotFound) {
		al.upstreamError(w, r, err)
		return
	}
	al.audit(r, p.user.Email, "alerts.silence_delete", id, matchersString(s.Matchers))
	w.WriteHeader(http.StatusNoContent)
}

// ---- conditions ----------------------------------------------------------------------------

type alertConditionJSON struct {
	Condition string              `json:"condition"`
	Label     string              `json:"label"`
	Kind      alerting.Kind       `json:"kind"`
	Scoped    bool                `json:"scoped"`
	Threshold *alerting.Threshold `json:"threshold,omitempty"`
	HasWindow bool                `json:"hasWindow"`
	Window    string              `json:"window,omitempty"` // default; absent: none (optional when hasWindow)
	For       string              `json:"for"`
	Severity  string              `json:"severity"`
	// OwnersOnly: only owners and admins may create such rules (Custom).
	OwnersOnly bool `json:"ownersOnly"`
}

// conditions lists what rules can watch, with their defaults, so editors
// show the same defaults the reconciler applies.
func (al *alertsAPI) conditions(w http.ResponseWriter, _ *http.Request) {
	out := make([]alertConditionJSON, 0, len(alerting.Catalog))
	for _, i := range alerting.Catalog {
		j := alertConditionJSON{Condition: string(i.Condition), Label: i.Label, Kind: i.Kind, Scoped: i.Scoped(), Threshold: i.Threshold,
			HasWindow: i.HasWindow, For: i.DefaultFor.String(), Severity: string(i.Severity), OwnersOnly: i.Kind == alerting.KindCustom}
		if i.DefaultWindow > 0 {
			j.Window = i.DefaultWindow.String()
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- rules -------------------------------------------------------------------------------------

type scopeJSON struct {
	Projects []string `json:"projects"`
	Apps     []string `json:"apps"`
}

type ruleJSON struct {
	Name          string    `json:"name"`
	Condition     string    `json:"condition"`
	Threshold     *int64    `json:"threshold,omitempty"`
	Window        string    `json:"window,omitempty"`
	For           string    `json:"for,omitempty"`
	Expr          string    `json:"expr,omitempty"`
	Scope         scopeJSON `json:"scope"`
	Severity      string    `json:"severity"`
	Channels      []string  `json:"channels"`
	Disabled      bool      `json:"disabled"`
	Default       bool      `json:"default"`
	Firing        int       `json:"firing"`
	Ready         bool      `json:"ready"`
	Message       string    `json:"message,omitempty"`
	EffectiveExpr string    `json:"effectiveExpr"`
	// Description says in plain words what the rule watches, defaults
	// filled in ("More than 5 restarts in 15 minutes, in all projects").
	Description string `json:"description"`
}

func ruleView(r *kwerftv1.AlertRule, firing int) ruleJSON {
	out := ruleJSON{
		Name: r.Name, Condition: string(r.Spec.Condition), Threshold: r.Spec.Threshold, Expr: r.Spec.Expr,
		Scope:    scopeJSON{Projects: nonNil(r.Spec.Scope.Projects), Apps: nonNil(r.Spec.Scope.Apps)},
		Severity: string(r.Spec.Severity), Channels: nonNil(r.Spec.Channels), Disabled: r.Spec.Disabled,
		Default: alerting.IsDefault(r), Firing: firing, EffectiveExpr: r.Status.Expr,
	}
	if r.Spec.Window != nil {
		out.Window = r.Spec.Window.Duration.String()
	}
	if r.Spec.For != nil {
		out.For = r.Spec.For.Duration.String()
	}
	if alerting.Validate(&r.Spec) == nil {
		out.EffectiveExpr = alerting.Expr(&r.Spec)
		out.Description = alerting.Describe(&r.Spec)
	}
	if c := meta.FindStatusCondition(r.Status.Conditions, controllers.ConditionReady); c != nil && c.ObservedGeneration == r.Generation {
		out.Message = c.Message
		out.Ready = c.Status == metav1.ConditionTrue
	} else {
		out.Message = "Updating the alert rules…"
	}
	return out
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// firingByRule counts the firing alerts the user sees, per rule. Best
// effort: without Alertmanager the counts are 0.
func (al *alertsAPI) firingByRule(ctx context.Context, v visibility) map[string]int {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out := map[string]int{}
	current, err := al.am.Alerts(ctx)
	if err != nil {
		return out
	}
	for _, a := range current {
		if len(a.Status.SilencedBy) == 0 && v.sees(a.Labels) {
			out[a.Labels[observability.LabelRule]]++
		}
	}
	return out
}

func ruleNotFound(name string) string { return fmt.Sprintf("Alert rule %q not found.", name) }

func (al *alertsAPI) rules(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	// Every role lists kwerft.dev resources (roles.yaml); rules carry no
	// secrets, so the cache may serve them.
	var list kwerftv1.AlertRuleList
	if err := al.api.list(ctx, c, &list); err != nil {
		al.kubeError(w, r, p, "alerts.rule_list", "", "Alert rules not found.", err)
		return
	}
	v, err := al.visibility(ctx, c, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.rule_list", "", "Projects not found.", err)
		return
	}
	firing := al.firingByRule(ctx, v)
	out := make([]ruleJSON, 0, len(list.Items))
	for i := range list.Items {
		if !v.seesRule(&list.Items[i].Spec) {
			continue // about projects the user does not reach
		}
		out = append(out, ruleView(&list.Items[i], firing[list.Items[i].Name]))
	}
	slices.SortFunc(out, func(a, b ruleJSON) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

type ruleInput struct {
	Name      string     `json:"name"`
	Condition string     `json:"condition"`
	Threshold *int64     `json:"threshold"`
	Window    string     `json:"window"`
	For       string     `json:"for"`
	Expr      string     `json:"expr"`
	Scope     *scopeJSON `json:"scope"`
	Severity  string     `json:"severity"`
	Channels  []string   `json:"channels"`
	Disabled  bool       `json:"disabled"`
}

// ruleSpec validates a RuleInput into a spec.
func ruleSpec(w http.ResponseWriter, in *ruleInput) (kwerftv1.AlertRuleSpec, bool) {
	spec := kwerftv1.AlertRuleSpec{
		Condition: kwerftv1.AlertCondition(strings.TrimSpace(in.Condition)), Threshold: in.Threshold,
		Expr: strings.TrimSpace(in.Expr), Severity: kwerftv1.AlertSeverity(strings.TrimSpace(in.Severity)), Disabled: in.Disabled,
	}
	for _, d := range []struct {
		field, raw string
		dst        **metav1.Duration
	}{{"window", in.Window, &spec.Window}, {"for", in.For, &spec.For}} {
		if strings.TrimSpace(d.raw) == "" {
			continue
		}
		v, err := alerting.ParseDuration(d.raw)
		if err != nil {
			writeFieldError(w, d.field, "Enter a duration like 30s, 15m, 1h or 7d.")
			return spec, false
		}
		*d.dst = alerting.Duration(v)
	}
	if spec.Severity == "" {
		if info, ok := alerting.Lookup(spec.Condition); ok {
			spec.Severity = info.Severity
		}
	}
	clean := func(in []string) []string {
		var out []string
		for _, s := range in {
			if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		slices.Sort(out)
		return out
	}
	if in.Scope != nil {
		spec.Scope = kwerftv1.AlertScope{Projects: clean(in.Scope.Projects), Apps: clean(in.Scope.Apps)}
	}
	spec.Channels = clean(in.Channels)
	if ferr := alerting.Validate(&spec); ferr != nil {
		writeFieldError(w, ferr.Field, ferr.Message)
		return spec, false
	}
	return spec, true
}

// customAllowed: Custom rules are owners' and admins' (RBAC cannot tell).
func (al *alertsAPI) customAllowed(w http.ResponseWriter, r *http.Request, p *principal, name string, specs ...kwerftv1.AlertRuleSpec) bool {
	if p.user.Role == store.RoleOwner || p.user.Role == store.RoleAdmin {
		return true
	}
	for _, s := range specs {
		if s.Condition == kwerftv1.AlertCustom {
			al.audit(r, p.user.Email, "alerts.rule.denied", name, "Custom rules are for owners and admins")
			writeError(w, http.StatusForbidden, "Custom expressions are for owners and admins. Choose one of the built-in conditions.")
			return false
		}
	}
	return true
}

// scopeAllowed: rules of developers watch only projects they work in
// (visibility.mayWriteRule), checked for the rule as it is and as it will be.
func (al *alertsAPI) scopeAllowed(w http.ResponseWriter, r *http.Request, ctx context.Context, p *principal, name string, specs ...kwerftv1.AlertRuleSpec) bool {
	if unconfined(p) {
		return true
	}
	v, err := al.visibility(ctx, nil, p)
	if err != nil {
		al.kubeError(w, r, p, "alerts.rule", name, "Projects not found.", err)
		return false
	}
	for i := range specs {
		if !v.mayWriteRule(&specs[i]) {
			al.audit(r, p.user.Email, "alerts.rule.denied", name, "watches projects beyond the user's")
			writeJSON(w, http.StatusForbidden, map[string]string{"field": "scope",
				"error": "Rules you write may only watch projects you work in. Pick them under Scope."})
			return false
		}
	}
	return true
}

func ruleDetail(spec kwerftv1.AlertRuleSpec) string {
	d := alerting.Describe(&spec) + "; severity " + string(spec.Severity)
	if len(spec.Channels) > 0 {
		d += "; channels " + strings.Join(spec.Channels, ", ")
	}
	if spec.Disabled {
		d += "; disabled"
	}
	if spec.Expr != "" {
		d += "; expr " + spec.Expr
	}
	return d
}

func (al *alertsAPI) ruleCreate(w http.ResponseWriter, r *http.Request) {
	var in ruleInput
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 63).")
		return
	}
	spec, ok := ruleSpec(w, &in)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	if !al.customAllowed(w, r, p, name, spec) || !al.scopeAllowed(w, r, ctx, p, name, spec) {
		return
	}
	rule := &kwerftv1.AlertRule{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	if err := c.Create(ctx, rule); err != nil {
		al.kubeError(w, r, p, "alerts.rule_create", name, ruleNotFound(name), err)
		return
	}
	al.audit(r, p.user.Email, "alerts.rule_create", name, ruleDetail(spec))
	writeJSON(w, http.StatusCreated, ruleView(rule, 0))
}

func (al *alertsAPI) ruleUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in ruleInput
	if !decode(w, r, &in) {
		return
	}
	if in.Name != "" && in.Name != name {
		writeFieldError(w, "name", "A rule cannot be renamed. Create a new one instead.")
		return
	}
	spec, ok := ruleSpec(w, &in)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	var rule kwerftv1.AlertRule
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &rule); err != nil {
		al.kubeError(w, r, p, "alerts.rule_update", name, ruleNotFound(name), err)
		return
	}
	if !al.customAllowed(w, r, p, name, rule.Spec, spec) || !al.scopeAllowed(w, r, ctx, p, name, rule.Spec, spec) {
		return
	}
	old := rule.Spec
	if err := updateSpec(ctx, c, &rule, func(r *kwerftv1.AlertRule) bool {
		if !equality.Semantic.DeepEqual(r.Spec, old) {
			return false
		}
		r.Spec = spec
		return true
	}); err != nil {
		al.kubeError(w, r, p, "alerts.rule_update", name, ruleNotFound(name), err)
		return
	}
	al.audit(r, p.user.Email, "alerts.rule_update", name, ruleDetail(spec))
	writeJSON(w, http.StatusOK, ruleView(&rule, 0))
}

// ruleDelete removes a rule. Default rules come back with their defaults,
// so deleting one resets it.
func (al *alertsAPI) ruleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	var rule kwerftv1.AlertRule
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &rule); err != nil {
		al.kubeError(w, r, p, "alerts.rule_delete", name, ruleNotFound(name), err)
		return
	}
	if !al.customAllowed(w, r, p, name, rule.Spec) || !al.scopeAllowed(w, r, ctx, p, name, rule.Spec) {
		return
	}
	uid := rule.UID
	if err := c.Delete(ctx, &rule, client.Preconditions{UID: &uid}); err != nil {
		al.kubeError(w, r, p, "alerts.rule_delete", name, ruleNotFound(name), err)
		return
	}
	detail := ""
	if alerting.IsDefault(&rule) {
		detail = "default rule; restored with its defaults"
	}
	al.audit(r, p.user.Email, "alerts.rule_delete", name, detail)
	w.WriteHeader(http.StatusNoContent)
}

// ---- channels -------------------------------------------------------------------------------

type slackJSON struct {
	Channel string `json:"channel"`
}

type emailJSON struct {
	To       []string `json:"to"`
	From     string   `json:"from"`
	SMTPHost string   `json:"smtpHost"`
	Username string   `json:"username,omitempty"`
}

type ntfyJSON struct {
	Server string `json:"server"`
	Topic  string `json:"topic"`
}

type channelJSON struct {
	Name          string     `json:"name"`
	Type          string     `json:"type"`
	Slack         *slackJSON `json:"slack,omitempty"`
	Email         *emailJSON `json:"email,omitempty"`
	Webhook       *struct{}  `json:"webhook,omitempty"`
	Ntfy          *ntfyJSON  `json:"ntfy,omitempty"`
	SendResolved  bool       `json:"sendResolved"`
	SecretSet     bool       `json:"secretSet"`
	Ready         bool       `json:"ready"`
	Message       string     `json:"message,omitempty"`
	LastTest      *time.Time `json:"lastTest,omitempty"`
	LastTestError string     `json:"lastTestError,omitempty"`
	// Rules are the rules that notify the channel.
	Rules []string `json:"rules"`
}

func channelView(ch *kwerftv1.NotificationChannel, rules []kwerftv1.AlertRule) channelJSON {
	out := channelJSON{
		Name: ch.Name, Type: string(ch.Spec.Type), SendResolved: ch.Spec.SendResolved == nil || *ch.Spec.SendResolved,
		SecretSet: ch.Status.SecretSet, LastTest: timePtr(ch.Status.LastTest), LastTestError: ch.Status.LastTestError, Rules: []string{},
	}
	switch {
	case ch.Spec.Slack != nil:
		out.Slack = &slackJSON{Channel: ch.Spec.Slack.Channel}
	case ch.Spec.Email != nil:
		e := ch.Spec.Email
		out.Email = &emailJSON{To: nonNil(e.To), From: e.From, SMTPHost: e.SMTPHost, Username: e.Username}
	case ch.Spec.Webhook != nil:
		out.Webhook = &struct{}{}
	case ch.Spec.Ntfy != nil:
		out.Ntfy = &ntfyJSON{Server: cmp.Or(ch.Spec.Ntfy.Server, "https://ntfy.sh"), Topic: ch.Spec.Ntfy.Topic}
	}
	for _, r := range rules {
		if slices.Contains(r.Spec.Channels, ch.Name) {
			out.Rules = append(out.Rules, r.Name)
		}
	}
	slices.Sort(out.Rules)
	if c := meta.FindStatusCondition(ch.Status.Conditions, controllers.ConditionReady); c != nil && c.ObservedGeneration == ch.Generation {
		out.Message = c.Message
		out.Ready = c.Status == metav1.ConditionTrue
	} else {
		out.Message = "Setting up the channel…"
	}
	return out
}

func channelNotFound(name string) string {
	return fmt.Sprintf("Notification channel %q not found.", name)
}

func (al *alertsAPI) channels(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	// As for rules: every role lists them, and they carry no secrets.
	var list kwerftv1.NotificationChannelList
	if err := al.api.list(ctx, c, &list); err != nil {
		al.kubeError(w, r, p, "alerts.channel_list", "", "Notification channels not found.", err)
		return
	}
	var rules kwerftv1.AlertRuleList
	if err := al.api.list(ctx, c, &rules); err != nil {
		al.kubeError(w, r, p, "alerts.channel_list", "", "Alert rules not found.", err)
		return
	}
	out := make([]channelJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, channelView(&list.Items[i], rules.Items))
	}
	slices.SortFunc(out, func(a, b channelJSON) int { return strings.Compare(a.Name, b.Name) })
	writeJSON(w, http.StatusOK, out)
}

type channelInput struct {
	Name         string     `json:"name"`
	Type         string     `json:"type"`
	Slack        *slackJSON `json:"slack"`
	Email        *emailJSON `json:"email"`
	Webhook      *struct{}  `json:"webhook"`
	Ntfy         *ntfyJSON  `json:"ntfy"`
	SendResolved *bool      `json:"sendResolved"`
	// Write-only secrets: the webhook URL (slack, webhook), the SMTP
	// password (email), the access token (ntfy). Empty keeps what is stored.
	URL      string `json:"url"`
	Password string `json:"password"`
	Token    string `json:"token"`
}

var (
	slackChannelName = regexp.MustCompile(`^[#@]?[a-z0-9][a-z0-9._-]{0,79}$`)
	ntfyTopic        = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// parsedChannel is a validated ChannelInput.
type parsedChannel struct {
	spec  kwerftv1.NotificationChannelSpec
	creds map[string]string // Secret key → value, only what was entered
}

// channelSpec validates a ChannelInput. old is the stored channel on update.
func channelSpec(w http.ResponseWriter, in *channelInput, old *kwerftv1.NotificationChannel) (*parsedChannel, bool) {
	out := &parsedChannel{creds: map[string]string{}}
	spec := &out.spec
	spec.Type = kwerftv1.NotificationType(strings.TrimSpace(in.Type))
	if old != nil && spec.Type != old.Spec.Type {
		writeFieldError(w, "type", "A channel's type cannot change. Create a new channel instead.")
		return nil, false
	}
	spec.SendResolved = boolPtr(in.SendResolved == nil || *in.SendResolved)
	urlField := func(raw string, httpsOnly bool, what string) bool {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return true
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || len(raw) > 2048 || strings.ContainsAny(raw, " \t\r\n") ||
			!(u.Scheme == "https" || !httpsOnly && u.Scheme == "http") {
			msg := "Enter the " + what + ", starting with https://."
			if !httpsOnly {
				msg = "Enter the " + what + ", starting with https:// or http://."
			}
			writeFieldError(w, "url", msg)
			return false
		}
		out.creds[observability.KeyURL] = raw
		return true
	}
	switch spec.Type {
	case kwerftv1.NotifySlack:
		s := &kwerftv1.SlackSettings{}
		if in.Slack != nil {
			s.Channel = strings.TrimSpace(in.Slack.Channel)
		}
		if s.Channel != "" && !slackChannelName.MatchString(s.Channel) {
			writeFieldError(w, "slack.channel", "Enter a channel like #ops-alerts, or leave it empty for the webhook's own channel.")
			return nil, false
		}
		spec.Slack = s
		if !urlField(in.URL, true, "Slack incoming webhook URL") {
			return nil, false
		}
	case kwerftv1.NotifyWebhook:
		spec.Webhook = &kwerftv1.WebhookSettings{}
		if !urlField(in.URL, false, "webhook URL") {
			return nil, false
		}
	case kwerftv1.NotifyEmail:
		e := in.Email
		if e == nil {
			e = &emailJSON{}
		}
		set := &kwerftv1.EmailSettings{From: strings.TrimSpace(e.From), SMTPHost: strings.TrimSpace(e.SMTPHost), Username: strings.TrimSpace(e.Username)}
		for _, to := range e.To {
			if to = strings.TrimSpace(to); to != "" && !slices.Contains(set.To, to) {
				if !validEmail(to) {
					writeFieldError(w, "email.to", fmt.Sprintf("%q is not an email address.", to))
					return nil, false
				}
				set.To = append(set.To, to)
			}
		}
		if len(set.To) == 0 || len(set.To) > 50 {
			writeFieldError(w, "email.to", "Enter at least one recipient.")
			return nil, false
		}
		if !validEmail(set.From) {
			writeFieldError(w, "email.from", "Enter the sender address, like kwerft@example.com.")
			return nil, false
		}
		host, port, err := net.SplitHostPort(set.SMTPHost)
		if n, perr := strconv.Atoi(port); err != nil || host == "" || perr != nil || n < 1 || n > 65535 || strings.ContainsAny(host, " /") {
			writeFieldError(w, "email.smtpHost", "Enter the SMTP server as host:port, like smtp.example.com:587.")
			return nil, false
		}
		if len(set.Username) > 256 || strings.ContainsAny(set.Username, "\r\n") {
			writeFieldError(w, "email.username", "That is not a username.")
			return nil, false
		}
		spec.Email = set
		if pw := in.Password; pw != "" {
			if len(pw) > 1024 || strings.ContainsAny(pw, "\r\n") {
				writeFieldError(w, "password", "That is not a password.")
				return nil, false
			}
			if set.Username == "" {
				writeFieldError(w, "password", "A password needs a username.")
				return nil, false
			}
			out.creds[observability.KeyPassword] = pw
		}
	case kwerftv1.NotifyNtfy:
		n := in.Ntfy
		if n == nil {
			n = &ntfyJSON{}
		}
		set := &kwerftv1.NtfySettings{Server: strings.TrimSuffix(strings.TrimSpace(n.Server), "/"), Topic: strings.TrimSpace(n.Topic)}
		if set.Server == "" {
			set.Server = "https://ntfy.sh"
		}
		if u, err := url.Parse(set.Server); err != nil || u.Scheme != "https" || u.Host == "" || u.RawQuery != "" || u.User != nil {
			writeFieldError(w, "ntfy.server", "Enter the ntfy server, like https://ntfy.sh.")
			return nil, false
		}
		if !ntfyTopic.MatchString(set.Topic) {
			writeFieldError(w, "ntfy.topic", "Enter a topic of letters, digits, - and _ (at most 64).")
			return nil, false
		}
		spec.Ntfy = set
		if tok := strings.TrimSpace(in.Token); tok != "" {
			if len(tok) > 256 || strings.ContainsAny(tok, " \t\r\n") {
				writeFieldError(w, "token", "That does not look like an access token.")
				return nil, false
			}
			out.creds[observability.KeyToken] = tok
		}
	default:
		writeFieldError(w, "type", "Choose Slack, email, webhook or ntfy.")
		return nil, false
	}
	// What the type needs must be entered on create, or when it is missing.
	if key, what := alerting.RequiredSecret(spec); key != "" && out.creds[key] == "" &&
		(old == nil || !old.Status.SecretSet || key == observability.KeyPassword && old.Spec.Email != nil && old.Spec.Email.Username != spec.Email.Username) {
		field := map[string]string{observability.KeyURL: "url", observability.KeyPassword: "password"}[key]
		writeFieldError(w, field, "Enter the "+what+".")
		return nil, false
	}
	return out, true
}

// writeChannelSecret merge-patches the channel's Secret as the user; nil
// removes a key. A new channel's Secret, and the user's access to it, appear
// a moment after the channel, so NotFound and Forbidden are retried.
func (al *alertsAPI) writeChannelSecret(ctx context.Context, c client.Client, name string, data map[string]*string) error {
	enc := map[string]any{}
	for k, v := range data {
		if v == nil {
			enc[k] = nil
		} else {
			enc[k] = base64.StdEncoding.EncodeToString([]byte(*v))
		}
	}
	raw, err := json.Marshal(map[string]any{"data": enc})
	if err != nil {
		return err
	}
	deadline := time.Now().Add(al.credentialsWait)
	for {
		// A fresh object each time: the answer is the whole Secret, which
		// must not linger.
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: observability.Namespace, Name: observability.ChannelSecret(name)}}
		err = c.Patch(ctx, sec, client.RawPatch(types.MergePatchType, raw))
		sec.Data = nil
		if err == nil || !(apierrors.IsNotFound(err) || apierrors.IsForbidden(err)) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// touchChannel tells the reconciler that credentials changed.
func (al *alertsAPI) touchChannel(ctx context.Context, c client.Client, name string) {
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		controllers.AnnotationCredentialsUpdated: al.now().UTC().Format(time.RFC3339Nano)}}})
	ch := &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Patch(ctx, ch, client.RawPatch(types.MergePatchType, raw)); err != nil {
		al.cfg.Logger.Error("could not mark channel credentials as updated", "channel", name, "err", err)
	}
}

func channelDetail(spec kwerftv1.NotificationChannelSpec, changed map[string]*string) string {
	d := string(spec.Type)
	switch {
	case spec.Slack != nil && spec.Slack.Channel != "":
		d += " " + spec.Slack.Channel
	case spec.Email != nil:
		d += " to " + strings.Join(spec.Email.To, ", ") + " via " + spec.Email.SMTPHost
	case spec.Ntfy != nil:
		d += " " + spec.Ntfy.Server + "/" + spec.Ntfy.Topic
	}
	var keys []string
	for k, v := range changed {
		if v != nil {
			keys = append(keys, k)
		} else {
			keys = append(keys, "removed "+k)
		}
	}
	if len(keys) > 0 {
		slices.Sort(keys)
		d += "; secret: " + strings.Join(keys, ", ")
	}
	return d
}

func (al *alertsAPI) channelCreate(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if !decode(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 || len(name) > 63-len(controllers.ChannelConfigPrefix) {
		writeFieldError(w, "name", "Use lowercase letters, digits and dashes, starting and ending with a letter or digit (at most 56).")
		return
	}
	parsed, ok := channelSpec(w, &in, nil)
	if !ok {
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	ch := &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: parsed.spec}
	if err := c.Create(ctx, ch); err != nil {
		al.kubeError(w, r, p, "alerts.channel_create", name, channelNotFound(name), err)
		return
	}
	al.audit(r, p.user.Email, "alerts.channel_create", name, channelDetail(parsed.spec, nil))
	if len(parsed.creds) > 0 {
		data := map[string]*string{}
		for k, v := range parsed.creds {
			data[k] = strPtr(v)
		}
		if err := al.writeChannelSecret(ctx, c, name, data); err != nil {
			al.cfg.Logger.Error("storing channel credentials failed", "channel", name, "err", err)
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				writeError(w, http.StatusServiceUnavailable, "The channel was created, but its credentials could not be stored yet. "+
					"Edit the channel and enter them again in a moment.")
				return
			}
			al.kubeError(w, r, p, "alerts.channel_credentials", name, channelNotFound(name), err)
			return
		}
		al.audit(r, p.user.Email, "alerts.channel_credentials", name, channelDetail(parsed.spec, data))
		al.touchChannel(ctx, c, name)
	}
	_ = c.Get(ctx, client.ObjectKey{Name: name}, ch)
	writeJSON(w, http.StatusCreated, channelView(ch, nil))
}

func (al *alertsAPI) channelUpdate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in channelInput
	if !decode(w, r, &in) {
		return
	}
	if in.Name != "" && in.Name != name {
		writeFieldError(w, "name", "A channel cannot be renamed. Create a new one instead.")
		return
	}
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	var ch kwerftv1.NotificationChannel
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ch); err != nil {
		al.kubeError(w, r, p, "alerts.channel_update", name, channelNotFound(name), err)
		return
	}
	parsed, ok := channelSpec(w, &in, &ch)
	if !ok {
		return
	}
	data := map[string]*string{}
	for k, v := range parsed.creds {
		data[k] = strPtr(v)
	}
	// An email channel without a username keeps no password.
	if e := parsed.spec.Email; e != nil && e.Username == "" && ch.Spec.Email != nil && ch.Spec.Email.Username != "" {
		data[observability.KeyPassword] = nil
	}
	old := ch.Spec
	if err := updateSpec(ctx, c, &ch, func(ch *kwerftv1.NotificationChannel) bool {
		if !equality.Semantic.DeepEqual(ch.Spec, old) {
			return false
		}
		ch.Spec = parsed.spec
		return true
	}); err != nil {
		al.kubeError(w, r, p, "alerts.channel_update", name, channelNotFound(name), err)
		return
	}
	al.audit(r, p.user.Email, "alerts.channel_update", name, channelDetail(parsed.spec, nil))
	if len(data) > 0 {
		if err := al.writeChannelSecret(ctx, c, name, data); err != nil {
			al.kubeError(w, r, p, "alerts.channel_credentials", name, "The channel's credential storage is not ready yet. Try again in a moment.", err)
			return
		}
		al.audit(r, p.user.Email, "alerts.channel_credentials", name, channelDetail(parsed.spec, data))
		al.touchChannel(ctx, c, name)
		_ = c.Get(ctx, client.ObjectKey{Name: name}, &ch)
	}
	var rules kwerftv1.AlertRuleList
	_ = al.api.list(ctx, c, &rules)
	writeJSON(w, http.StatusOK, channelView(&ch, rules.Items))
}

func (al *alertsAPI) channelDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	// Straight from the API server: a rule naming the channel a moment ago
	// must still stop the delete.
	var rules kwerftv1.AlertRuleList
	if err := c.List(ctx, &rules); err != nil {
		al.kubeError(w, r, p, "alerts.channel_delete", name, "Alert rules not found.", err)
		return
	}
	var users []string
	for _, rule := range rules.Items {
		if slices.Contains(rule.Spec.Channels, name) {
			users = append(users, rule.Name)
		}
	}
	if len(users) > 0 {
		slices.Sort(users)
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "Alert rules still notify this channel: " + strings.Join(users, ", ") + ". Remove it from them first.",
			"rules": users,
		})
		return
	}
	// The reconciler (and garbage collection) remove the Secret and config.
	if err := c.Delete(ctx, &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		al.kubeError(w, r, p, "alerts.channel_delete", name, channelNotFound(name), err)
		return
	}
	al.audit(r, p.user.Email, "alerts.channel_delete", name, "")
	w.WriteHeader(http.StatusNoContent)
}

// channelTest sends a test notification straight to the channel's
// destination and records the outcome in the channel's status.
func (al *alertsAPI) channelTest(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	c, p, ctx, cancel, err := al.userClient(r)
	defer cancel()
	if err != nil {
		al.internalError(w, r, err)
		return
	}
	var ch kwerftv1.NotificationChannel
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &ch); err != nil {
		al.kubeError(w, r, p, "alerts.channel_test", name, channelNotFound(name), err)
		return
	}
	if al.cfg.SystemReader == nil {
		writeError(w, http.StatusServiceUnavailable, "This console cannot read channel credentials (no cluster identity of its own).")
		return
	}
	var sec corev1.Secret
	if err := al.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: observability.Namespace, Name: observability.ChannelSecret(name)}, &sec); err != nil && !apierrors.IsNotFound(err) {
		al.internalError(w, r, err)
		return
	}
	testErr := al.notifier.Test(ctx, &ch, sec.Data, al.consoleDomain())
	sec.Data = nil
	now := metav1.NewTime(al.now().UTC())
	status := map[string]any{"lastTest": now, "lastTestError": nil}
	msg := "Sent. Check that it arrived."
	if testErr != nil {
		msg = testErr.Error()
		status["lastTestError"] = msg
	}
	raw, _ := json.Marshal(map[string]any{"status": status})
	if err := c.Status().Patch(ctx, &kwerftv1.NotificationChannel{ObjectMeta: metav1.ObjectMeta{Name: name}}, client.RawPatch(types.MergePatchType, raw)); err != nil {
		al.cfg.Logger.Error("could not record the test result", "channel", name, "err", err)
	}
	detail := "ok"
	if testErr != nil {
		detail = "failed: " + msg
	}
	al.audit(r, p.user.Email, "alerts.channel_test", name, detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": testErr == nil, "message": msg})
}

// updateSpec writes a spec change to obj, which the caller just read.
// Reconcilers write status and annotations all the time, and any such write
// makes an update from the copy just read conflict. updateSpec then reads
// the object again and applies change to the fresh copy, unless change
// refuses it (returns false: the spec itself changed meanwhile, so the
// conflict is real).
func updateSpec[T client.Object](ctx context.Context, c client.Client, obj T, change func(T) bool) error {
	key := client.ObjectKeyFromObject(obj)
	for attempt := 0; ; attempt++ {
		if !change(obj) {
			return apierrors.NewConflict(schema.GroupResource{Group: kwerftv1.GroupVersion.Group}, key.Name, errors.New("changed meanwhile"))
		}
		err := c.Update(ctx, obj)
		if !apierrors.IsConflict(err) || attempt == 4 {
			return err
		}
		// Decoding into a used object would keep fields the fresh copy lacks.
		reflect.ValueOf(obj).Elem().SetZero()
		if err := c.Get(ctx, key, obj); err != nil {
			return err
		}
	}
}
