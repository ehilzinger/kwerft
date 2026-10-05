package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
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
	"github.com/ehilzinger/kwerft/internal/access"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/upgrades"
	"github.com/ehilzinger/kwerft/internal/version"
)

// Upgrades from the console (docs/phase6-upgrades.md › API and UI):
// Settings › Updates. Owners start and cancel upgrades and change the update
// policy; owners and admins read them and ask for a release check. Every
// request acts as the signed-in user, in the cluster it names (?cluster=,
// or "cluster" in the body; default the local one), so Kubernetes RBAC
// decides as well (roles.yaml: admins only read Upgrades).
//
// Starting an upgrade takes the password (or an authenticator code) and,
// for a Kubernetes minor, the target version typed again: k3s cannot be
// rolled back. The preflight the Upgrade controller runs is run first,
// synchronously (Config.Upgrades), and blocking checks refuse the request.
//
// Audited: upgrade.start, upgrade.cancel, updates.policy,
// updates.autopatch_resumed.

// UpgradePreflight runs the checks the Upgrade controller runs before it
// changes anything, for an Upgrade that does not exist yet: the dialog's
// live preflight and POST /api/v1/upgrades. cmd/kwerft builds it from
// controllers.UpgradeChecks.
type UpgradePreflight interface {
	// Supports reports whether upgrades of the cluster (clusters.Local or a
	// remote cluster's name) can be started from the console.
	Supports(cluster string) bool
	// Preflight checks spec in the cluster; ErrUpgradeUnsupported when
	// Supports is false.
	Preflight(ctx context.Context, cluster string, spec kwerftv1.UpgradeSpec) ([]kwerftv1.UpgradeCheck, error)
}

// ErrUpgradeUnsupported: this console cannot start upgrades of the cluster.
var ErrUpgradeUnsupported = errors.New("upgrades of this cluster cannot be started from the console")

type upgradesAPI struct {
	*api
	// poll is how often a live status stream rereads the Upgrade; ping
	// keeps it open through proxies; maxStream ends it (the client
	// reconnects).
	poll, ping, maxStream time.Duration
	streams               *slots
	preflights            *limiter
}

func (a *api) registerUpgrades(mux *http.ServeMux) {
	u := &upgradesAPI{api: a, poll: 2 * time.Second, ping: 15 * time.Second, maxStream: time.Hour,
		streams: newSlots(8, 200), preflights: newLimiter(60, 15*time.Minute, a.now)}
	if a.cfg.upgradesHook != nil {
		a.cfg.upgradesHook(u)
	}
	read := func(h http.HandlerFunc) http.HandlerFunc {
		return a.requireUser(a.requireKube(a.requireRole(a.withClusterParam(h), access.RolesWith(access.ReadUpdates)...)))
	}
	readWrite := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(read(h)) }
	owner := func(h http.HandlerFunc) http.HandlerFunc {
		return a.sameOrigin(a.requireUser(a.requireKube(a.requireRole(a.withClusterParam(h), access.RolesWith(access.Upgrades)...))))
	}

	mux.HandleFunc("GET /api/v1/updates", read(u.overview))
	mux.HandleFunc("POST /api/v1/updates/check", readWrite(u.check))
	mux.HandleFunc("POST /api/v1/updates/resume-autopatch", owner(u.resumeAutoPatch))
	mux.HandleFunc("PUT /api/v1/settings/updates", owner(u.setPolicy))

	mux.HandleFunc("GET /api/v1/upgrades", read(u.list))
	mux.HandleFunc("POST /api/v1/upgrades/preflight", owner(u.preflight))
	mux.HandleFunc("POST /api/v1/upgrades", owner(u.start))
	mux.HandleFunc("GET /api/v1/upgrades/{name}", read(u.get))
	mux.HandleFunc("GET /api/v1/upgrades/{name}/events", read(u.events))
	mux.HandleFunc("GET /api/v1/upgrades/{name}/log", read(u.log))
	mux.HandleFunc("DELETE /api/v1/upgrades/{name}", owner(u.cancel))
}

// ---- views ------------------------------------------------------------------------------

type upgradeCheckJSON struct {
	Check   string `json:"check"`
	OK      bool   `json:"ok"`
	Warning bool   `json:"warning,omitempty"`
	Message string `json:"message,omitempty"`
}

type upgradeStepJSON struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	State  string     `json:"state"` // Running, Done, Skipped, Failed
	Detail string     `json:"detail,omitempty"`
	At     *time.Time `json:"at,omitempty"`
}

type upgradeNodeJSON struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	State   string `json:"state"` // Waiting, Draining, Upgrading, Done, Failed
	Message string `json:"message,omitempty"`
}

type versionsJSON struct {
	Kwerft     string `json:"kwerft,omitempty"`
	Kubernetes string `json:"kubernetes,omitempty"`
}

type upgradeBackupJSON struct {
	EtcdSnapshot  string           `json:"etcdSnapshot,omitempty"`
	Database      string           `json:"database,omitempty"`
	HelmRevisions map[string]int32 `json:"helmRevisions,omitempty"`
}

// upgradeJSON is one Upgrade as Settings › Updates shows it.
type upgradeJSON struct {
	Name      string `json:"name"`
	Cluster   string `json:"cluster"`
	Component string `json:"component"`
	Version   string `json:"version"`
	// RequestedBy: the owner's email, or "auto-update" (Auto).
	RequestedBy        string `json:"requestedBy,omitempty"`
	Auto               bool   `json:"auto"`
	AcceptDataRollback bool   `json:"acceptDataRollback,omitempty"`
	// Phase: Pending (not picked up yet), Queued, Preflight, Backup,
	// Running, Verifying, RollingBack, Succeeded, RolledBack, Failed,
	// Cancelled.
	Phase     string             `json:"phase"`
	From      *versionsJSON      `json:"from,omitempty"`
	Preflight []upgradeCheckJSON `json:"preflight"`
	Steps     []upgradeStepJSON  `json:"steps"`
	Nodes     []upgradeNodeJSON  `json:"nodes"`
	Backup    *upgradeBackupJSON `json:"backup,omitempty"`
	Reason    string             `json:"reason,omitempty"`
	Message   string             `json:"message,omitempty"`
	// CancelRequestedBy: an owner asked to cancel; the controller decides.
	CancelRequestedBy string     `json:"cancelRequestedBy,omitempty"`
	Cancellable       bool       `json:"cancellable"`
	Finished          bool       `json:"finished"`
	CreatedAt         time.Time  `json:"createdAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
}

// cancellable: only before the installer (Kwerft) or the Plans
// (Kubernetes) start, i.e. before Running.
func cancellable(p kwerftv1.UpgradePhase) bool {
	switch p {
	case "", kwerftv1.UpgradeQueued, kwerftv1.UpgradePreflight, kwerftv1.UpgradeBackingUp:
		return true
	}
	return false
}

func checksView(in []kwerftv1.UpgradeCheck) []upgradeCheckJSON {
	out := make([]upgradeCheckJSON, 0, len(in))
	for _, c := range in {
		out = append(out, upgradeCheckJSON{Check: c.Check, OK: c.OK, Warning: c.Warning, Message: c.Message})
	}
	return out
}

func upgradeView(cluster string, u *kwerftv1.Upgrade) upgradeJSON {
	st := u.Status
	out := upgradeJSON{
		Name: u.Name, Cluster: cluster, Component: string(u.Spec.Component), Version: u.Spec.Version,
		RequestedBy: u.Annotations[kwerftv1.AnnotationRequestedBy], AcceptDataRollback: u.Spec.AcceptDataRollback,
		Phase: cmp.Or(string(st.Phase), "Pending"), Preflight: checksView(st.Preflight),
		Steps: []upgradeStepJSON{}, Nodes: []upgradeNodeJSON{},
		Reason: st.Reason, Message: st.Message, CancelRequestedBy: u.Annotations[kwerftv1.AnnotationCancelRequested],
		Cancellable: cancellable(st.Phase) && u.DeletionTimestamp.IsZero(), Finished: upgrades.Finished(st.Phase),
		CreatedAt: u.CreationTimestamp.UTC(), StartedAt: timePtr(st.StartedAt), FinishedAt: timePtr(st.FinishedAt),
	}
	out.Auto = out.RequestedBy == kwerftv1.RequestedByAutoUpdate
	if st.From != nil {
		out.From = &versionsJSON{Kwerft: st.From.Kwerft, Kubernetes: st.From.Kubernetes}
	}
	for _, s := range st.Steps {
		out.Steps = append(out.Steps, upgradeStepJSON{ID: s.ID, Label: s.Label, State: s.State, Detail: s.Detail, At: timePtr(s.At)})
	}
	for _, n := range st.Nodes {
		out.Nodes = append(out.Nodes, upgradeNodeJSON{Name: n.Name, Version: n.Version, State: n.State, Message: n.Message})
	}
	if b := st.Backup; b != nil {
		out.Backup = &upgradeBackupJSON{EtcdSnapshot: b.EtcdSnapshot, Database: b.Database, HelmRevisions: b.HelmRevisions}
	}
	return out
}

// newestFirst orders Upgrades by creation, newest first.
func newestFirst(a, b upgradeJSON) int {
	if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(b.Name, a.Name)
}

type windowJSON struct {
	// Days: Mon … Sun; empty: every day.
	Days  []string `json:"days"`
	Start string   `json:"start"` // HH:MM
	// Duration like 2h or 1h30m.
	Duration string `json:"duration"`
	TimeZone string `json:"timeZone,omitempty"` // IANA; empty: UTC
}

type updatePolicyJSON struct {
	Policy            string      `json:"policy"`  // Off | Notify | AutoPatch
	Channel           string      `json:"channel"` // stable | edge
	KubernetesPatches bool        `json:"kubernetesPatches"`
	Window            *windowJSON `json:"window,omitempty"`
}

type availableJSON struct {
	Component string `json:"component"`
	Version   string `json:"version"`
	Kind      string `json:"kind"` // Patch | Minor
	// Notes are the release notes (Markdown) without the installer's
	// "Install" section.
	Notes   string `json:"notes,omitempty"`
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// clusterUpdatesJSON is one cluster's row of the versions table.
type clusterUpdatesJSON struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	// Kwerft: the console's version (local) or the agent's.
	Kwerft string `json:"kwerft,omitempty"`
	// Kubernetes: the oldest kubelet.
	Kubernetes string          `json:"kubernetes,omitempty"`
	Available  []availableJSON `json:"available"`
	// Active is the newest Upgrade that has not finished.
	Active *upgradeJSON `json:"active,omitempty"`
	// Upgradable: upgrades of this cluster can be started from here.
	Upgradable bool   `json:"upgradable"`
	Message    string `json:"message,omitempty"`
}

type updatesJSON struct {
	Policy updatePolicyJSON `json:"policy"`
	// CheckedAt is the last release check; Checking: a check was asked
	// for after it.
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Checking  bool       `json:"checking"`
	Error     string     `json:"error,omitempty"`
	// AutoPatchPausedBy names the auto-update that failed or rolled back.
	AutoPatchPausedBy string `json:"autoPatchPausedBy,omitempty"`
	// NextWindow: when the maintenance window opens next (AutoPatch).
	NextWindow *time.Time   `json:"nextWindow,omitempty"`
	WindowOpen bool         `json:"windowOpen"`
	Current    versionsJSON `json:"current"`
	// Available are the local cluster's targets, newest first.
	Available []availableJSON      `json:"available"`
	Clusters  []clusterUpdatesJSON `json:"clusters"`
	// CanUpgrade: the user may start upgrades and change the policy.
	CanUpgrade bool `json:"canUpgrade"`
}

func formatDuration(d time.Duration) string {
	s := d.Round(time.Minute).String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func policyView(s *kwerftv1.UpdateSettings) updatePolicyJSON {
	out := updatePolicyJSON{Policy: string(kwerftv1.UpdatesNotify), Channel: upgrades.ChannelStable}
	if s == nil {
		return out
	}
	out.Policy = cmp.Or(string(s.Policy), out.Policy)
	out.Channel = cmp.Or(s.Channel, out.Channel)
	out.KubernetesPatches = s.KubernetesPatches
	if w := s.Window; w != nil {
		d := upgrades.DefaultWindowDuration
		if w.Duration != nil {
			d = w.Duration.Duration
		}
		out.Window = &windowJSON{Days: append([]string{}, w.Days...), Start: w.Start, Duration: formatDuration(d), TimeZone: w.TimeZone}
	}
	return out
}

// installSection is the installer's part of NOTES.md (hack/release.sh
// notes): how to install, the image and chart. The console upgrades
// itself, so Settings › Updates leaves it out.
var installSection = regexp.MustCompile(`(?ms)^## Install[ \t]*\n.*?(?:^## |\z)`)

// ReleaseNotes strips the "## Install" section from a release's NOTES.md.
func ReleaseNotes(notes string) string {
	out := installSection.ReplaceAllStringFunc(notes, func(m string) string {
		if strings.HasSuffix(m, "## ") {
			return "## "
		}
		return ""
	})
	return strings.TrimSpace(out)
}

func availableView(in []kwerftv1.AvailableUpdate) []availableJSON {
	out := make([]availableJSON, 0, len(in))
	for _, a := range in {
		out = append(out, availableJSON{Component: string(a.Component), Version: a.Version, Kind: a.Kind,
			Notes: ReleaseNotes(a.Notes), Allowed: a.Allowed, Reason: a.Reason})
	}
	return out
}

// runningKwerft is the console's own release.
func runningKwerft() string { return strings.TrimPrefix(version.Version, "v") }

// oldestKubelet is the oldest kubelet version of the nodes ("" when none
// parses).
func oldestKubelet(nodes []corev1.Node) string {
	var oldest *upgrades.Version
	for _, n := range nodes {
		v, err := upgrades.ParseVersion(n.Status.NodeInfo.KubeletVersion)
		if err != nil {
			continue
		}
		if oldest == nil || v.Less(*oldest) {
			oldest = &v
		}
	}
	if oldest == nil {
		return ""
	}
	return oldest.String()
}

// ---- reading ------------------------------------------------------------------------------

// settings reads the management cluster's ConsoleSettings as the user;
// nil when there are none yet.
func (u *upgradesAPI) settings(ctx context.Context, c client.Client) (*kwerftv1.ConsoleSettings, error) {
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

// management is the user's client in the local cluster with a bounded
// context, whatever cluster the request names: update settings exist only
// there.
func (u *upgradesAPI) management(r *http.Request) (client.Client, *principal, context.Context, context.CancelFunc, error) {
	p := principalOf(r)
	c, err := u.managementClient(p)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	return c, p, ctx, cancel, err
}

// clusterUpgrades lists a cluster's Upgrades as the user, newest first. A
// cluster without the Upgrade kind (an older agent) has none.
func clusterUpgrades(ctx context.Context, c client.Client, cluster string) ([]upgradeJSON, error) {
	var list kwerftv1.UpgradeList
	if err := c.List(ctx, &list); err != nil {
		if meta.IsNoMatchError(err) {
			return []upgradeJSON{}, nil
		}
		return nil, err
	}
	out := make([]upgradeJSON, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, upgradeView(cluster, &list.Items[i]))
	}
	slices.SortFunc(out, newestFirst)
	return out, nil
}

func activeOf(list []upgradeJSON) *upgradeJSON {
	for i := range list {
		if !list[i].Finished {
			return &list[i]
		}
	}
	return nil
}

func (u *upgradesAPI) overview(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := u.management(r)
	defer cancel()
	if err != nil {
		u.internalError(w, r, err)
		return
	}
	cs, err := u.settings(ctx, c)
	if err != nil {
		u.kubeError(w, r, p, "updates.read", "updates", "Settings not found.", err)
		return
	}
	out := updatesJSON{Available: []availableJSON{}, Clusters: []clusterUpdatesJSON{},
		CanUpgrade: access.Allowed(p.user.Role, access.Upgrades), Current: versionsJSON{Kwerft: runningKwerft()}}
	var spec *kwerftv1.UpdateSettings
	if cs != nil {
		spec = cs.Spec.Updates
	}
	out.Policy = policyView(spec)
	if cs != nil && cs.Status.Updates != nil {
		st := cs.Status.Updates
		out.CheckedAt, out.Error, out.AutoPatchPausedBy = timePtr(st.CheckedAt), st.Error, st.AutoPatchPausedBy
		if out.Policy.Policy != string(kwerftv1.UpdatesOff) {
			out.Available = availableView(st.Available)
		}
	}
	if cs != nil {
		if at, err := time.Parse(time.RFC3339, cs.Annotations[kwerftv1.AnnotationCheckUpdatesRequested]); err == nil &&
			out.Policy.Policy != string(kwerftv1.UpdatesOff) && (out.CheckedAt == nil || at.After(*out.CheckedAt)) {
			out.Checking = true
		}
	}
	if spec != nil && spec.Window != nil {
		if win, err := upgrades.ParseWindow(spec.Window); err == nil && win != nil {
			now := u.now()
			out.WindowOpen, _ = win.Open(now)
			if next := win.Next(now); !next.IsZero() {
				next = next.UTC()
				out.NextWindow = &next
			}
		}
	}

	// Per cluster: what runs, what could, and what is under way.
	var remote kwerftv1.ClusterList
	if err := c.List(ctx, &remote); err != nil && !meta.IsNoMatchError(err) && !apierrors.IsForbidden(err) {
		u.kubeError(w, r, p, "updates.read", "updates", "Clusters not found.", err)
		return
	}
	for _, st := range u.clusters.states() {
		row := clusterUpdatesJSON{Name: st.name, Connected: st.conn != nil, Available: []availableJSON{}}
		row.Upgradable = u.cfg.Upgrades != nil && u.cfg.Upgrades.Supports(st.name)
		if st.name == clusters.Local {
			row.Kwerft = runningKwerft()
			row.Available = out.Available
		} else if i := slices.IndexFunc(remote.Items, func(cl kwerftv1.Cluster) bool { return cl.Name == st.name }); i >= 0 {
			cl := remote.Items[i].Status
			row.Kwerft, row.Kubernetes = strings.TrimPrefix(cl.AgentVersion, "v"), cl.KubernetesVersion
			// Agents follow the console: its release is their target
			// (docs/phase6-upgrades.md › Agent clusters).
			if t := agentTarget(row.Kwerft); t != nil {
				row.Available = append(row.Available, *t)
			}
		}
		if st.conn == nil {
			row.Message = "Not connected: its agent is not reachable right now."
			out.Clusters = append(out.Clusters, row)
			continue
		}
		cctx := withCluster(ctx, st.conn, true)
		uc, err := st.conn.kube.For(p.user.Email, p.user.Role)
		if err != nil {
			u.internalError(w, r, err)
			return
		}
		var nodes corev1.NodeList
		if err := uc.List(cctx, &nodes); err == nil {
			if k := oldestKubelet(nodes.Items); k != "" {
				row.Kubernetes = k
			}
		} else if st.name == clusters.Local {
			u.kubeError(w, r, p, "updates.read", "updates", "Nodes not found.", err)
			return
		}
		if list, err := clusterUpgrades(cctx, uc, st.name); err == nil {
			row.Active = activeOf(list)
		} else if st.name == clusters.Local {
			u.kubeError(w, r, p, "updates.read", "updates", "Upgrades not found.", err)
			return
		} else {
			row.Message = "Its upgrades cannot be read right now."
		}
		if st.name == clusters.Local {
			out.Current.Kubernetes = row.Kubernetes
		}
		out.Clusters = append(out.Clusters, row)
	}
	writeJSON(w, http.StatusOK, out)
}

// agentTarget: an agent behind the console is offered the console's own
// release.
func agentTarget(agent string) *availableJSON {
	console := runningKwerft()
	if !upgrades.IsRelease(console) {
		return nil
	}
	cv, err1 := upgrades.ParseVersion(console)
	av, err2 := upgrades.ParseVersion(agent)
	if err1 != nil || err2 != nil || !av.Less(cv) {
		return nil
	}
	return &availableJSON{Component: string(kwerftv1.UpgradeKwerft), Version: console, Kind: upgrades.UpgradeKind(av, cv), Allowed: true}
}

func (u *upgradesAPI) list(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	var conns []*clusterConn
	var unreachable []string
	if sel := selectedCluster(r.Context()); sel != nil {
		conns = []*clusterConn{sel}
	} else {
		for _, st := range u.clusters.states() {
			if st.conn != nil {
				conns = append(conns, st.conn)
			} else {
				unreachable = append(unreachable, st.name)
			}
		}
	}
	out := []upgradeJSON{}
	for _, conn := range conns {
		cctx := withCluster(ctx, conn, true)
		c, err := conn.kube.For(p.user.Email, p.user.Role)
		if err != nil {
			u.internalError(w, r, err)
			return
		}
		list, err := clusterUpgrades(cctx, c, conn.name)
		if err != nil {
			if conn.isLocal() || len(conns) == 1 {
				u.kubeError(w, r.WithContext(cctx), p, "upgrade.list", "upgrades", "Upgrades not found.", err)
				return
			}
			unreachable = append(unreachable, conn.name)
			continue
		}
		out = append(out, list...)
	}
	slices.SortFunc(out, newestFirst)
	if len(unreachable) > 0 {
		w.Header().Set(unreachableHeader, strings.Join(unreachable, ","))
	}
	writeJSON(w, http.StatusOK, out)
}

// one reads the named Upgrade as the user in the request's cluster. On
// false it has answered.
func (u *upgradesAPI) one(w http.ResponseWriter, r *http.Request) (*kwerftv1.Upgrade, client.Client, bool) {
	c, p, ctx, cancel, err := u.userClient(r)
	defer cancel()
	if err != nil {
		u.internalError(w, r, err)
		return nil, nil, false
	}
	name := r.PathValue("name")
	var up kwerftv1.Upgrade
	if err := c.Get(ctx, client.ObjectKey{Name: name}, &up); err != nil {
		if meta.IsNoMatchError(err) {
			err = apierrors.NewNotFound(kwerftv1.GroupVersion.WithResource("upgrades").GroupResource(), name)
		}
		u.kubeError(w, r, p, "upgrade.read", name, fmt.Sprintf("Upgrade %q not found.", name), err)
		return nil, nil, false
	}
	return &up, c, true
}

func (u *upgradesAPI) get(w http.ResponseWriter, r *http.Request) {
	up, _, ok := u.one(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, upgradeView(u.conn(r.Context()).name, up))
}

// log is the installer log's tail (ConfigMap kwerft-system/<upgrade>-log,
// key install.log), read with the console's own identity once the user's
// may read the Upgrade: no console role reads kwerft-system.
func (u *upgradesAPI) log(w http.ResponseWriter, r *http.Request) {
	up, _, ok := u.one(w, r)
	if !ok {
		return
	}
	conn := u.conn(r.Context())
	if conn.systemReader == nil {
		writeError(w, http.StatusServiceUnavailable, "The installer log is not available on this console.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	var cm corev1.ConfigMap
	err := conn.systemReader.Get(ctx, client.ObjectKey{Namespace: controllers.GatewayNamespace, Name: upgrades.LogConfigMapName(up.Name)}, &cm)
	switch {
	case apierrors.IsNotFound(err):
		writeJSON(w, http.StatusOK, map[string]any{"log": "", "truncated": false})
		return
	case err != nil:
		u.kubeError(w, r, principalOf(r), "upgrade.log", up.Name, "The log was not found.", err)
		return
	}
	text := cm.Data[upgrades.LogKey]
	writeJSON(w, http.StatusOK, map[string]any{"log": text, "truncated": len(text) >= upgrades.LogTailBytes})
}

// events streams an Upgrade's status as Server-Sent Events: "upgrade" with
// the whole view whenever it changed (the first right away), "end" once it
// finished, "gone" when it was deleted. Comments keep the connection open;
// the stream ends after maxStream or when the session does, and the page
// reconnects (also across the console's own restart).
func (u *upgradesAPI) events(w http.ResponseWriter, r *http.Request) {
	up, c, ok := u.one(w, r)
	if !ok {
		return
	}
	pr := principalOf(r)
	release := u.streams.acquire(pr.user.Email)
	if release == nil {
		writeError(w, http.StatusTooManyRequests, "Too many upgrade views are open. Close one and try again.")
		return
	}
	defer release()
	cluster := u.conn(r.Context()).name
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	out := &sseWriter{w: w, rc: http.NewResponseController(w), timeout: 10 * time.Second}
	send := func(name string, v any) bool { return out.event(name, v) == nil && out.flush() == nil }

	if !send("upgrade", upgradeView(cluster, up)) {
		return
	}
	if upgrades.Finished(up.Status.Phase) {
		send("end", map[string]string{"phase": string(up.Status.Phase)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), u.maxStream)
	defer cancel()
	tick, ping := time.NewTicker(u.poll), time.NewTicker(u.ping)
	defer tick.Stop()
	defer ping.Stop()
	seen, checked := up.ResourceVersion, u.now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if out.comment("ping") != nil {
				return
			}
		case <-tick.C:
			if u.now().Sub(checked) > 30*time.Second {
				checked = u.now()
				if !u.stillValid(ctx, pr) {
					send("end", map[string]string{"reason": "session"})
					return
				}
			}
			gctx, gcancel := context.WithTimeout(ctx, kubeTimeout)
			var cur kwerftv1.Upgrade
			err := c.Get(gctx, client.ObjectKey{Name: up.Name}, &cur)
			gcancel()
			switch {
			case apierrors.IsNotFound(err):
				send("gone", map[string]string{"name": up.Name})
				return
			case err != nil:
				// The API server or the cluster's tunnel is away for a
				// moment (an upgrade restarts things); try again.
				continue
			case cur.ResourceVersion == seen:
				continue
			}
			seen = cur.ResourceVersion
			if !send("upgrade", upgradeView(cluster, &cur)) {
				return
			}
			if upgrades.Finished(cur.Status.Phase) {
				send("end", map[string]string{"phase": string(cur.Status.Phase)})
				return
			}
		}
	}
}

// ---- starting ---------------------------------------------------------------------------

type upgradeRequest struct {
	Cluster            string `json:"cluster"`
	Component          string `json:"component"`
	Version            string `json:"version"`
	AcceptDataRollback bool   `json:"acceptDataRollback"`
	// Password (or a current authenticator code) and, for a Kubernetes
	// minor, the target version typed again.
	Password       string `json:"password"`
	ConfirmVersion string `json:"confirmVersion"`
}

// target is what an upgrade request is about, resolved.
type upgradeTarget struct {
	conn *clusterConn
	ctx  context.Context // carries the cluster
	c    client.Client   // the user's, in that cluster
	spec kwerftv1.UpgradeSpec
	// from is the running version of the component; kind Patch or Minor.
	from, kind string
}

// needsTypedConfirmation: a Kubernetes minor upgrade (or one whose running
// version is unknown), which cannot be rolled back.
func (t *upgradeTarget) needsTypedConfirmation() bool {
	return t.spec.Component == kwerftv1.UpgradeKubernetes && t.kind != "Patch"
}

// resolve validates the request and reads the running version as the
// user. On false it has answered.
func (u *upgradesAPI) resolve(w http.ResponseWriter, r *http.Request, req *upgradeRequest, ctx context.Context) (*upgradeTarget, bool) {
	p := principalOf(r)
	req.Version = strings.TrimSpace(req.Version)
	comp := kwerftv1.UpgradeComponent(req.Component)
	if comp != kwerftv1.UpgradeKwerft && comp != kwerftv1.UpgradeKubernetes {
		writeFieldError(w, "component", "Choose Kwerft or Kubernetes.")
		return nil, false
	}
	v, err := upgrades.ParseVersion(req.Version)
	if err != nil || len(req.Version) > 64 {
		writeFieldError(w, "version", "Enter a version, like 0.6.0 or v1.38.1+k3s1.")
		return nil, false
	}
	if comp == kwerftv1.UpgradeKubernetes && !strings.HasPrefix(req.Version, "v") {
		writeFieldError(w, "version", "Enter a k3s version, like v1.38.1+k3s1.")
		return nil, false
	}
	cluster := cmp.Or(req.Cluster, clusters.Local)
	conn, err := u.clusters.byName(cluster)
	if err != nil {
		clusterError(w, err)
		return nil, false
	}
	if u.cfg.Upgrades == nil || !u.cfg.Upgrades.Supports(conn.name) {
		writeError(w, http.StatusConflict, "Upgrades of cluster "+conn.name+" cannot be started from this console. Re-run the installer of the release on its server instead.")
		return nil, false
	}
	t := &upgradeTarget{conn: conn, ctx: withCluster(ctx, conn, true),
		spec: kwerftv1.UpgradeSpec{Component: comp, Version: v.String(), AcceptDataRollback: req.AcceptDataRollback}}
	if t.c, err = conn.kube.For(p.user.Email, p.user.Role); err != nil {
		u.internalError(w, r, err)
		return nil, false
	}
	if comp == kwerftv1.UpgradeKwerft {
		t.from = runningKwerft()
		if !conn.isLocal() {
			t.from = ""
			var cl kwerftv1.Cluster
			if mc, err := u.managementClient(p); err == nil {
				gctx, cancel := context.WithTimeout(ctx, kubeTimeout)
				if mc.Get(gctx, client.ObjectKey{Name: conn.name}, &cl) == nil {
					t.from = strings.TrimPrefix(cl.Status.AgentVersion, "v")
				}
				cancel()
			}
		}
	} else {
		var nodes corev1.NodeList
		lctx, cancel := context.WithTimeout(t.ctx, kubeTimeout)
		err := t.c.List(lctx, &nodes)
		cancel()
		if err != nil {
			u.kubeError(w, r.WithContext(t.ctx), p, "upgrade.start", "nodes", "Nodes not found.", err)
			return nil, false
		}
		t.from = oldestKubelet(nodes.Items)
	}
	t.kind = "Minor"
	if from, err := upgrades.ParseVersion(t.from); err == nil {
		t.kind = upgrades.UpgradeKind(from, v)
	}
	return t, true
}

type preflightJSON struct {
	Cluster   string             `json:"cluster"`
	Component string             `json:"component"`
	Version   string             `json:"version"`
	From      string             `json:"from,omitempty"`
	Kind      string             `json:"kind"`
	Checks    []upgradeCheckJSON `json:"checks"`
	// Blocked: a failed check that is not a warning; the upgrade would be
	// refused.
	Blocked bool `json:"blocked"`
	// ConfirmVersion: the version must be typed again (Kubernetes minor).
	ConfirmVersion bool `json:"confirmVersion"`
	// DataRollback: the release is not rollback-safe; starting needs
	// acceptDataRollback.
	DataRollback bool `json:"dataRollback"`
}

// runPreflight runs the synchronous preflight. On false it has answered.
func (u *upgradesAPI) runPreflight(w http.ResponseWriter, r *http.Request, t *upgradeTarget) (preflightJSON, bool) {
	out := preflightJSON{Cluster: t.conn.name, Component: string(t.spec.Component), Version: t.spec.Version, From: t.from, Kind: t.kind,
		ConfirmVersion: t.needsTypedConfirmation()}
	if !u.preflights.allow(principalOf(r).user.ID) {
		writeError(w, http.StatusTooManyRequests, "Too many checks. Wait a few minutes and try again.")
		return out, false
	}
	// The checks read the install repository and the registry: give them
	// longer than one Kubernetes request.
	ctx, cancel := context.WithTimeout(t.ctx, 45*time.Second)
	defer cancel()
	checks, err := u.cfg.Upgrades.Preflight(ctx, t.conn.name, t.spec)
	if errors.Is(err, ErrUpgradeUnsupported) {
		writeError(w, http.StatusConflict, "Upgrades of cluster "+t.conn.name+" cannot be started from this console.")
		return out, false
	}
	if err != nil {
		u.internalError(w, r, err)
		return out, false
	}
	out.Checks = checksView(checks)
	out.Blocked = len(controllers.Blocked(checks)) > 0
	for _, c := range checks {
		if c.Check == controllers.CheckDataRollback && (!c.OK || strings.HasPrefix(c.Message, "Accepted")) {
			out.DataRollback = true
		}
	}
	return out, true
}

func (u *upgradesAPI) preflight(w http.ResponseWriter, r *http.Request) {
	var req upgradeRequest
	if !decode(w, r, &req) {
		return
	}
	t, ok := u.resolve(w, r, &req, r.Context())
	if !ok {
		return
	}
	out, ok := u.runPreflight(w, r, t)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (u *upgradesAPI) start(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	var req upgradeRequest
	if !decode(w, r, &req) {
		return
	}
	t, ok := u.resolve(w, r, &req, r.Context())
	if !ok {
		return
	}
	if t.needsTypedConfirmation() && strings.TrimSpace(req.ConfirmVersion) != t.spec.Version {
		writeFieldError(w, "confirmVersion", "Type "+t.spec.Version+" to confirm: Kubernetes cannot be rolled back automatically.")
		return
	}
	if !u.confirmIdentity(w, r, p.user, req.Password) {
		return
	}
	// One unfinished Upgrade per component and cluster: a second one would
	// only fail its preflight once the first is done.
	ctx, cancel := context.WithTimeout(t.ctx, kubeTimeout)
	existing, err := clusterUpgrades(ctx, t.c, t.conn.name)
	cancel()
	if err != nil {
		u.kubeError(w, r.WithContext(t.ctx), p, "upgrade.start", "upgrades", "Upgrades not found.", err)
		return
	}
	for _, e := range existing {
		if !e.Finished && e.Component == string(t.spec.Component) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   fmt.Sprintf("Upgrade %s to %s is %s. Wait for it or cancel it first.", e.Name, e.Version, strings.ToLower(e.Phase)),
				"upgrade": e,
			})
			return
		}
	}
	pf, ok := u.runPreflight(w, r, t)
	if !ok {
		return
	}
	if pf.Blocked {
		var msgs []string
		for _, c := range pf.Checks {
			if !c.OK && !c.Warning {
				msgs = append(msgs, c.Message)
			}
		}
		writeJSON(w, http.StatusConflict, map[string]any{"error": "The preflight failed: " + strings.Join(msgs, " "), "preflight": pf})
		return
	}
	up := &kwerftv1.Upgrade{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: controllers.GenerateName(t.spec.Component, t.spec.Version),
			Annotations:  map[string]string{kwerftv1.AnnotationRequestedBy: p.user.Email},
		},
		Spec: t.spec,
	}
	ctx, cancel = context.WithTimeout(t.ctx, kubeTimeout)
	defer cancel()
	if err := t.c.Create(ctx, up); err != nil {
		u.kubeError(w, r.WithContext(t.ctx), p, "upgrade.start", string(t.spec.Component)+" "+t.spec.Version, "Upgrade not found.", err)
		return
	}
	detail := fmt.Sprintf("%s %s → %s on %s", t.spec.Component, cmp.Or(t.from, "?"), t.spec.Version, t.conn.name)
	if t.spec.AcceptDataRollback {
		detail += "; data rollback accepted"
	}
	u.audit(r, p.user.Email, "upgrade.start", up.Name, detail)
	writeJSON(w, http.StatusCreated, map[string]any{"upgrade": upgradeView(t.conn.name, up), "preflight": pf})
}

// cancel asks the controller to stop an Upgrade that has not reached
// Running: the annotation keeps the record, and the phase's optimistic lock
// decides a race with the runner.
func (u *upgradesAPI) cancel(w http.ResponseWriter, r *http.Request) {
	up, c, ok := u.one(w, r)
	if !ok {
		return
	}
	p := principalOf(r)
	if !cancellable(up.Status.Phase) || !up.DeletionTimestamp.IsZero() {
		msg := fmt.Sprintf("Upgrade %s is %s and can no longer be cancelled.", up.Name, strings.ToLower(string(up.Status.Phase)))
		if !upgrades.Finished(up.Status.Phase) {
			msg = fmt.Sprintf("Upgrade %s is %s: it can no longer be cancelled, and rolls back by itself if it fails.", up.Name, strings.ToLower(string(up.Status.Phase)))
		}
		writeError(w, http.StatusConflict, msg)
		return
	}
	cluster := u.conn(r.Context()).name
	if by := up.Annotations[kwerftv1.AnnotationCancelRequested]; by != "" {
		writeJSON(w, http.StatusAccepted, upgradeView(cluster, up))
		return
	}
	raw, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": up.ResourceVersion,
		"annotations":     map[string]string{kwerftv1.AnnotationCancelRequested: p.user.Email},
	}})
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	if err := c.Patch(ctx, up, client.RawPatch(types.MergePatchType, raw)); err != nil {
		u.kubeError(w, r, p, "upgrade.cancel", up.Name, fmt.Sprintf("Upgrade %q not found.", up.Name), err)
		return
	}
	u.audit(r, p.user.Email, "upgrade.cancel", up.Name, fmt.Sprintf("%s %s on %s, %s", up.Spec.Component, up.Spec.Version, cluster,
		strings.ToLower(cmp.Or(string(up.Status.Phase), "pending"))))
	writeJSON(w, http.StatusAccepted, upgradeView(cluster, up))
}

// ---- policy -------------------------------------------------------------------------------

var (
	dayNames = []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}
	startRE  = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
)

// normalizeWindow checks a window; on a problem it returns the field and
// message.
func normalizeWindow(in *windowJSON) (*kwerftv1.MaintenanceWindow, string, string) {
	out := &kwerftv1.MaintenanceWindow{Start: strings.TrimSpace(in.Start), TimeZone: strings.TrimSpace(in.TimeZone)}
	for _, d := range in.Days {
		i := slices.IndexFunc(dayNames, func(n string) bool { return strings.EqualFold(n, strings.TrimSpace(d)) })
		if i < 0 {
			return nil, "window.days", "Days are Mon, Tue, Wed, Thu, Fri, Sat and Sun."
		}
		if !slices.Contains(out.Days, dayNames[i]) {
			out.Days = append(out.Days, dayNames[i])
		}
	}
	slices.SortFunc(out.Days, func(a, b string) int { return slices.Index(dayNames, a) - slices.Index(dayNames, b) })
	if len(out.Days) == len(dayNames) {
		out.Days = nil // every day
	}
	if !startRE.MatchString(out.Start) {
		return nil, "window.start", "Enter the start as HH:MM, like 03:00."
	}
	d := upgrades.DefaultWindowDuration
	if s := strings.TrimSpace(in.Duration); s != "" {
		var err error
		if d, err = time.ParseDuration(s); err != nil || d < 30*time.Minute || d > 24*time.Hour || d%time.Minute != 0 {
			return nil, "window.duration", "Enter a duration from 30m to 24h, like 2h or 1h30m."
		}
	}
	out.Duration = &metav1.Duration{Duration: d}
	if out.TimeZone != "" {
		if _, err := time.LoadLocation(out.TimeZone); err != nil || len(out.TimeZone) > 64 {
			return nil, "window.timeZone", "Enter a time zone like Europe/Berlin, or leave it empty for UTC."
		}
	}
	if _, err := upgrades.ParseWindow(out); err != nil {
		return nil, "window", "This window is not valid: " + err.Error() + "."
	}
	return out, "", ""
}

func describePolicy(p updatePolicyJSON) string {
	s := p.Policy + ", channel " + p.Channel
	if p.KubernetesPatches {
		s += ", Kubernetes patches"
	}
	if w := p.Window; w != nil {
		days := "every day"
		if len(w.Days) > 0 {
			days = strings.Join(w.Days, ",")
		}
		s += fmt.Sprintf(", window %s %s for %s %s", days, w.Start, w.Duration, cmp.Or(w.TimeZone, "UTC"))
	}
	return s
}

func (u *upgradesAPI) setPolicy(w http.ResponseWriter, r *http.Request) {
	var req updatePolicyJSON
	if !decode(w, r, &req) {
		return
	}
	switch kwerftv1.UpdatePolicy(req.Policy) {
	case kwerftv1.UpdatesOff, kwerftv1.UpdatesNotify, kwerftv1.UpdatesAutoPatch:
	default:
		writeFieldError(w, "policy", "Choose Off, Notify or AutoPatch.")
		return
	}
	req.Channel = cmp.Or(strings.TrimSpace(req.Channel), upgrades.ChannelStable)
	if req.Channel != upgrades.ChannelStable && req.Channel != "edge" {
		writeFieldError(w, "channel", "Choose the stable or the edge channel.")
		return
	}
	spec := &kwerftv1.UpdateSettings{Channel: req.Channel, Policy: kwerftv1.UpdatePolicy(req.Policy), KubernetesPatches: req.KubernetesPatches}
	if req.Window != nil {
		win, field, msg := normalizeWindow(req.Window)
		if win == nil {
			writeFieldError(w, field, msg)
			return
		}
		spec.Window = win
	}
	if spec.Policy == kwerftv1.UpdatesAutoPatch && spec.Window == nil {
		writeFieldError(w, "window", "AutoPatch installs patch releases only inside a maintenance window. Set one.")
		return
	}
	c, p, ctx, cancel, err := u.management(r)
	defer cancel()
	if err != nil {
		u.internalError(w, r, err)
		return
	}
	cs, err := u.settings(ctx, c)
	if err != nil {
		u.kubeError(w, r, p, "updates.policy", "updates", "Settings not found.", err)
		return
	}
	before := policyView(nil)
	if cs != nil {
		before = policyView(cs.Spec.Updates)
	}
	if err := patchConsoleSettings(ctx, c, cs, map[string]any{"spec": map[string]any{"updates": updatesPatch(spec)}}); err != nil {
		u.kubeError(w, r, p, "updates.policy", "updates", "Settings not found.", err)
		return
	}
	after := policyView(spec)
	u.audit(r, p.user.Email, "updates.policy", "updates", describePolicy(before)+" → "+describePolicy(after))
	writeJSON(w, http.StatusOK, after)
}

// updatesPatch is spec.updates as a merge patch that replaces every field:
// what the request leaves out (a window, a time zone, Kubernetes patches)
// is removed, not kept from before.
func updatesPatch(s *kwerftv1.UpdateSettings) map[string]any {
	out := map[string]any{"channel": s.Channel, "policy": s.Policy, "kubernetesPatches": s.KubernetesPatches, "window": nil}
	if w := s.Window; w != nil {
		win := map[string]any{"days": nil, "start": w.Start, "duration": nil, "timeZone": nil}
		if len(w.Days) > 0 {
			win["days"] = w.Days
		}
		if w.Duration != nil {
			win["duration"] = w.Duration
		}
		if w.TimeZone != "" {
			win["timeZone"] = w.TimeZone
		}
		out["window"] = win
	}
	return out
}

// check asks the Updates reconciler for a release check now (owners and
// admins: it changes nothing but the time of the next check).
func (u *upgradesAPI) check(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := u.management(r)
	defer cancel()
	if err != nil {
		u.internalError(w, r, err)
		return
	}
	cs, err := u.settings(ctx, c)
	if err != nil {
		u.kubeError(w, r, p, "updates.check", "updates", "Settings not found.", err)
		return
	}
	if cs != nil && policyView(cs.Spec.Updates).Policy == string(kwerftv1.UpdatesOff) {
		writeError(w, http.StatusConflict, "Updates are off: this console makes no requests for releases. Choose Notify or AutoPatch first.")
		return
	}
	now := u.now().UTC().Format(time.RFC3339)
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{kwerftv1.AnnotationCheckUpdatesRequested: now}}}
	if err := patchConsoleSettings(ctx, c, cs, patch); err != nil {
		u.kubeError(w, r, p, "updates.check", "updates", "Settings not found.", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"requestedAt": now})
}

// resumeAutoPatch clears the pause an auto-update that failed or rolled
// back put on AutoPatch (the Updates reconciler acts on the annotation).
func (u *upgradesAPI) resumeAutoPatch(w http.ResponseWriter, r *http.Request) {
	c, p, ctx, cancel, err := u.management(r)
	defer cancel()
	if err != nil {
		u.internalError(w, r, err)
		return
	}
	cs, err := u.settings(ctx, c)
	if err != nil {
		u.kubeError(w, r, p, "updates.resume", "updates", "Settings not found.", err)
		return
	}
	if cs == nil || cs.Status.Updates == nil || cs.Status.Updates.AutoPatchPausedBy == "" {
		writeError(w, http.StatusConflict, "AutoPatch is not paused.")
		return
	}
	by := cs.Status.Updates.AutoPatchPausedBy
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]string{kwerftv1.AnnotationResumeAutoPatch: by}}}
	if err := patchConsoleSettings(ctx, c, cs, patch); err != nil {
		u.kubeError(w, r, p, "updates.resume", "updates", "Settings not found.", err)
		return
	}
	u.audit(r, p.user.Email, "updates.autopatch_resumed", "updates", "paused by "+by)
	writeJSON(w, http.StatusAccepted, map[string]string{"resumed": by})
}
