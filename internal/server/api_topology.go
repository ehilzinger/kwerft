package server

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hubble"
	"github.com/ehilzinger/kwerft/internal/metrics"
)

// Topology: the Overview's infrastructure map (docs/plan.md, "Overview
// map"). One cluster's projects, apps and the servers their replicas run
// on, scheduled jobs and recent tasks, volumes, domains, traffic rules with
// their counts, dropped connections, servers and the server firewall: what
// the Apps, Jobs, Volumes, Network and Clusters pages show, in one answer
// the map polls.
//
// Each part is read the way its own page reads it, so the map never shows
// more than those pages would:
//   - Apps, Schedules, Tasks, Volumes, Domains and TrafficRules: lists from
//     the informer cache confined to the user's project scope (scopedList),
//     like the list endpoints.
//   - Pods (where replicas run): as the signed-in user, like the replicas
//     table. A project whose pods the user may not read shows its apps
//     without placement (placed: false).
//   - Dropped connections: Hubble's, confined to the scope (trafficDrops).
//   - Servers and the firewall: owners and admins only, as on their pages;
//     everyone else gets just the names of the servers their pods run on.
//   - Volume fill and server usage: VictoriaMetrics, confined like the
//     metrics pages; left out when monitoring does not answer.

const (
	// recentTasks: one-off tasks finished longer ago than this stay off the map.
	recentTasks = 24 * time.Hour
	// maxTasksPerProject keeps a project that runs many one-off tasks readable.
	maxTasksPerProject = 5
	// topologyMetricsTimeout bounds the optional VictoriaMetrics queries.
	topologyMetricsTimeout = 5 * time.Second
)

func (a *api) registerTopology(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/topology", a.requireUser(a.requireKube(a.withClusterParam(a.topology))))
}

type topologyJSON struct {
	Cluster   string             `json:"cluster"`
	Projects  []topoProjectJSON  `json:"projects"`
	Apps      []topoAppJSON      `json:"apps"`
	Schedules []topoScheduleJSON `json:"schedules"`
	Tasks     []topoTaskJSON     `json:"tasks"`
	Volumes   []topoVolumeJSON   `json:"volumes"`
	Domains   []domainJSON       `json:"domains"`
	Rules     []topoRuleJSON     `json:"rules"`
	Drops     []dropJSON         `json:"drops"`
	Hubble    hubbleJSON         `json:"hubble"`
	// Nodes: every server for owners and admins; for everyone else only the
	// servers their pods run on, by name (NodesPartial).
	Nodes        []topoNodeJSON `json:"nodes"`
	NodesPartial bool           `json:"nodesPartial"`
	// Firewall is null for those the firewall page is not for.
	Firewall []firewallRuleJSON `json:"firewall"`
	// Metrics: volume fill (and, for owners and admins, server usage) came
	// from VictoriaMetrics.
	Metrics bool `json:"metrics"`
}

type topoProjectJSON struct {
	Name     string `json:"name"`
	Isolated bool   `json:"isolated"`
	// Placed: the user may read the project's pods, so apps carry where
	// their replicas run.
	Placed bool `json:"placed"`
}

type topoPodJSON struct {
	Name     string `json:"name"`
	Node     string `json:"node,omitempty"`
	Status   string `json:"status"`
	Tone     string `json:"tone"`
	Ready    bool   `json:"ready"`
	Restarts int32  `json:"restarts"`
}

type topoMountJSON struct {
	// Volume is a Volume's name, or "<app>/data-<n>" for the app's own disk.
	Volume   string `json:"volume"`
	Path     string `json:"path"`
	ReadOnly bool   `json:"readOnly"`
}

type topoAppJSON struct {
	appSummaryJSON
	Ports     []kwerftv1.AppPort `json:"ports"`
	AllowFrom []string           `json:"allowFrom"`
	Egress    string             `json:"egress"`
	Mounts    []topoMountJSON    `json:"mounts"`
	Pods      []topoPodJSON      `json:"pods"`
}

type topoScheduleJSON struct {
	scheduleJSON
	// Node ran the last run, when its pod is still there to say so.
	Node string `json:"node,omitempty"`
}

type topoTaskJSON struct {
	taskJSON
	Node string `json:"node,omitempty"`
}

type topoVolumeJSON struct {
	volumeJSON
	// Own: an app's own disk (a volume entry without a Volume object).
	Own bool   `json:"own"`
	App string `json:"app,omitempty"`
	// Node is the server of a pod that mounts it.
	Node          string   `json:"node,omitempty"`
	UsedBytes     *float64 `json:"usedBytes,omitempty"`
	CapacityBytes *float64 `json:"capacityBytes,omitempty"`
	// claim is the PersistentVolumeClaim the metrics are labelled with.
	claim string
}

type topoRuleJSON struct {
	trafficRuleJSON
	Project string `json:"project"`
}

type topoNodeJSON struct {
	nodeJSON
	// ServerType is the node pool's Hetzner server type, e.g. cpx31.
	ServerType string `json:"serverType,omitempty"`
	// Usage from VictoriaMetrics; nil without it.
	Usage *nodeUsageJSON `json:"usage,omitempty"`
}

func (a *api) topology(w http.ResponseWriter, r *http.Request) {
	p := principalOf(r)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	conn := a.conn(ctx)
	ctx = withCluster(ctx, conn, true)
	r = r.WithContext(ctx)
	c, err := conn.kube.For(p.user.Email, p.user.Role)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	scope, projects, err := a.scopeAndProjects(ctx, p)
	if err == nil {
		var out *topologyJSON
		if out, err = a.buildTopology(ctx, c, p, scope, projects); err == nil {
			writeJSON(w, http.StatusOK, out)
			return
		}
	}
	a.kubeError(w, r, p, "topology.read", "", "No projects found.", err)
}

func (a *api) buildTopology(ctx context.Context, c client.Client, p *principal, scope *projectScope, projects []kwerftv1.Project) (*topologyJSON, error) {
	conn := a.conn(ctx)
	out := &topologyJSON{
		Cluster: conn.name, Projects: []topoProjectJSON{}, Apps: []topoAppJSON{}, Schedules: []topoScheduleJSON{},
		Tasks: []topoTaskJSON{}, Volumes: []topoVolumeJSON{}, Domains: []domainJSON{}, Rules: []topoRuleJSON{},
		Drops: []dropJSON{}, Hubble: a.hubbleStatus(ctx), Nodes: []topoNodeJSON{},
	}

	var (
		apps      kwerftv1.AppList
		schedules kwerftv1.ScheduleList
		tasks     kwerftv1.TaskList
		volumes   kwerftv1.VolumeList
		domains   kwerftv1.DomainList
		rules     kwerftv1.TrafficRuleList
	)
	for _, l := range []client.ObjectList{&apps, &schedules, &tasks, &volumes, &domains, &rules} {
		if err := a.scopedList(ctx, c, scope, l); err != nil {
			return nil, err
		}
	}
	pods, placed, err := a.topologyPods(ctx, c, scope)
	if err != nil {
		return nil, err
	}

	for i := range projects {
		pr := &projects[i]
		if !scope.reaches(pr.Name) || !scope.has(pr.Name) {
			continue
		}
		out.Projects = append(out.Projects, topoProjectJSON{Name: pr.Name, Isolated: pr.Spec.Isolated == nil || *pr.Spec.Isolated, Placed: placed[pr.Name]})
	}
	slices.SortFunc(out.Projects, func(x, y topoProjectJSON) int { return strings.Compare(x.Name, y.Name) })

	// Pods by app and by task; claims to the node of a pod that mounts them.
	byApp := map[string][]topoPodJSON{}
	taskNode := map[string]string{}
	claimNode := map[string]string{}
	now := a.now()
	for i := range pods {
		pod := &pods[i]
		key := func(name string) string { return pod.Namespace + "/" + name }
		if app := pod.Labels[controllers.LabelApp]; app != "" && pod.Labels[controllers.LabelTask] == "" {
			s := podSummary(pod, now)
			byApp[key(app)] = append(byApp[key(app)], topoPodJSON{Name: s.Name, Node: s.Node, Status: s.Status, Tone: s.Tone, Ready: s.Ready, Restarts: s.Restarts})
		}
		if t := pod.Labels[controllers.LabelTask]; t != "" && pod.Spec.NodeName != "" {
			taskNode[key(t)] = pod.Spec.NodeName
		}
		for _, v := range pod.Spec.Volumes {
			if pvc := v.PersistentVolumeClaim; pvc != nil && pod.Spec.NodeName != "" && pod.Status.Phase == corev1.PodRunning {
				claimNode[key(pvc.ClaimName)] = pod.Spec.NodeName
			}
		}
	}

	for i := range apps.Items {
		app := &apps.Items[i]
		ta := topoAppJSON{appSummaryJSON: appSummary(app), Ports: nonNil(app.Spec.Ports), AllowFrom: nonNil(app.Spec.AllowFrom),
			Egress: cmp.Or(app.Spec.Egress, "https"), Mounts: []topoMountJSON{}, Pods: nonNil(byApp[app.Namespace+"/"+app.Name])}
		ta.Cluster = conn.name
		slices.SortFunc(ta.Pods, func(x, y topoPodJSON) int { return strings.Compare(x.Name, y.Name) })
		disk := 0
		for _, v := range app.Spec.Volumes {
			switch {
			case v.Secret != "":
				continue // a file from a secret set, not storage
			case v.Volume != "":
				ta.Mounts = append(ta.Mounts, topoMountJSON{Volume: v.Volume, Path: v.Path, ReadOnly: v.ReadOnly})
			default:
				name := fmt.Sprintf("%s/data-%d", app.Name, disk)
				// The StatefulSet's claim of replica 0: data-<n>-<app>-0.
				claim := fmt.Sprintf("data-%d-%s-0", disk, app.Name)
				disk++
				ta.Mounts = append(ta.Mounts, topoMountJSON{Volume: name, Path: v.Path, ReadOnly: v.ReadOnly})
				out.Volumes = append(out.Volumes, topoVolumeJSON{
					volumeJSON: volumeJSON{Name: name, Project: app.Namespace, Cluster: conn.name, Size: v.Size.String(), Class: v.Class,
						Phase: "bound", UsedBy: []string{app.Name}, Created: app.CreationTimestamp.UTC()},
					Own: true, App: app.Name, Node: claimNode[app.Namespace+"/"+claim], claim: claim,
				})
			}
		}
		out.Apps = append(out.Apps, ta)
	}

	latest := latestScheduledRuns(tasks.Items)
	for i := range schedules.Items {
		s := &schedules.Items[i]
		last := latest[s.Namespace+"/"+s.Name]
		ts := topoScheduleJSON{scheduleJSON: scheduleSummary(s, last)}
		ts.Cluster = conn.name
		if last != nil {
			ts.Node = taskNode[last.Namespace+"/"+last.Name]
		}
		out.Schedules = append(out.Schedules, ts)
	}
	perProject := map[string]int{}
	oneOff := slices.DeleteFunc(slices.Clone(tasks.Items), func(t kwerftv1.Task) bool {
		if t.Labels[controllers.LabelSchedule] != "" {
			return true
		}
		phase := taskPhase(&t)
		return (phase == "succeeded" || phase == "failed") && now.Sub(taskEnd(&t)) > recentTasks
	})
	slices.SortFunc(oneOff, func(x, y kwerftv1.Task) int { return y.CreationTimestamp.Compare(x.CreationTimestamp.Time) })
	for i := range oneOff {
		t := &oneOff[i]
		if perProject[t.Namespace]++; perProject[t.Namespace] > maxTasksPerProject {
			continue
		}
		tt := topoTaskJSON{taskJSON: taskSummary(t), Node: taskNode[t.Namespace+"/"+t.Name]}
		tt.Cluster = conn.name
		out.Tasks = append(out.Tasks, tt)
	}

	for i := range volumes.Items {
		v := &volumes.Items[i]
		tv := topoVolumeJSON{volumeJSON: volumeSummary(v), claim: cmp.Or(v.Status.ClaimName, v.Name)}
		tv.Cluster = conn.name
		tv.Node = claimNode[v.Namespace+"/"+tv.claim]
		out.Volumes = append(out.Volumes, tv)
	}
	for i := range domains.Items {
		d := domainSummary(&domains.Items[i])
		d.Cluster = conn.name
		out.Domains = append(out.Domains, d)
	}
	flows := conn.hubble
	for i := range rules.Items {
		out.Rules = append(out.Rules, topoRuleJSON{trafficRuleJSON: trafficRuleSummary(&rules.Items[i], flows), Project: rules.Items[i].Namespace})
	}
	if flows != nil {
		hs := hubble.Unconfined()
		if !scope.platform {
			hs = hubble.Namespaces(scope.namespaces()...)
		}
		out.Drops = dropsJSON(flows.Drops(hs, nil, maxDrops), hs)
	}

	if err := a.topologyNodes(ctx, c, p, scope, out, pods); err != nil {
		return nil, err
	}
	if scope.platform {
		var fw kwerftv1.FirewallRuleList
		if err := a.list(ctx, c, &fw); err != nil {
			return nil, err
		}
		out.Firewall = make([]firewallRuleJSON, 0, len(fw.Items))
		for i := range fw.Items {
			out.Firewall = append(out.Firewall, fwRuleJSON(&fw.Items[i]))
		}
		sortFirewallRules(out.Firewall)
	}
	a.topologyMetrics(ctx, scope, out)

	slices.SortFunc(out.Apps, func(x, y topoAppJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	slices.SortFunc(out.Schedules, func(x, y topoScheduleJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	slices.SortFunc(out.Volumes, func(x, y topoVolumeJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	slices.SortFunc(out.Domains, func(x, y domainJSON) int { return byProjectAndName(x.Project, x.Hostname, y.Project, y.Hostname) })
	slices.SortFunc(out.Rules, func(x, y topoRuleJSON) int { return byProjectAndName(x.Project, x.Name, y.Project, y.Name) })
	return out, nil
}

// latestScheduledRuns maps "<namespace>/<schedule>" to its newest run, as
// the Jobs page does.
func latestScheduledRuns(tasks []kwerftv1.Task) map[string]*kwerftv1.Task {
	latest := map[string]*kwerftv1.Task{}
	for i := range tasks {
		t := &tasks[i]
		s := t.Labels[controllers.LabelSchedule]
		if s == "" {
			continue
		}
		key := t.Namespace + "/" + s
		if cur := latest[key]; cur == nil || cur.CreationTimestamp.Before(&t.CreationTimestamp) ||
			(cur.CreationTimestamp.Equal(&t.CreationTimestamp) && cur.Name < t.Name) {
			latest[key] = t
		}
	}
	return latest
}

// taskEnd is when a task finished, or was created when it never started.
func taskEnd(t *kwerftv1.Task) time.Time {
	if f := t.Status.CompletionTime; f != nil {
		return f.Time
	}
	return t.CreationTimestamp.Time
}

// topologyPods lists the pods of the user's projects as the user, project
// by project: pod access is bound per project namespace, for owners and
// admins too (kwerft:pods-read). placed says for which projects Kubernetes
// let the user read them.
func (a *api) topologyPods(ctx context.Context, c client.Client, scope *projectScope) ([]corev1.Pod, map[string]bool, error) {
	placed := map[string]bool{}
	var all []corev1.Pod
	for _, ns := range scope.namespaces() {
		var list corev1.PodList
		err := c.List(ctx, &list, client.InNamespace(ns), client.HasLabels{controllers.LabelProject})
		if apierrors.IsForbidden(err) {
			continue // the role may not read pods there: apps without placement
		}
		if err != nil {
			return nil, nil, err
		}
		placed[ns] = true
		all = append(all, list.Items...)
	}
	return all, placed, nil
}

// topologyNodes fills in the servers: all of them, with their pool's server
// type, for owners and admins; otherwise the names their pods run on.
func (a *api) topologyNodes(ctx context.Context, c client.Client, p *principal, scope *projectScope, out *topologyJSON, pods []corev1.Pod) error {
	if !scope.platform {
		out.NodesPartial = true
		seen := map[string]bool{}
		for _, pod := range pods {
			if n := pod.Spec.NodeName; n != "" && !seen[n] {
				seen[n] = true
				out.Nodes = append(out.Nodes, topoNodeJSON{nodeJSON: nodeJSON{Name: n, Roles: []string{}, Ready: true, Status: "Ready"}})
			}
		}
		slices.SortFunc(out.Nodes, func(x, y topoNodeJSON) int { return strings.Compare(x.Name, y.Name) })
		return nil
	}
	var nodes corev1.NodeList
	if err := c.List(ctx, &nodes); err != nil {
		return err
	}
	types := map[string]string{}
	if mc, err := a.managementClient(p); err == nil {
		var pools kwerftv1.NodePoolList
		if err := mc.List(ctx, &pools); err == nil {
			for _, pool := range pools.Items {
				if pool.Spec.Cluster == out.Cluster {
					types[pool.Name] = pool.Spec.ServerType
				}
			}
		}
	}
	for i := range nodes.Items {
		n := &nodes.Items[i]
		out.Nodes = append(out.Nodes, topoNodeJSON{nodeJSON: toNodeJSON(n), ServerType: types[n.Labels[hetzner.LabelPool]]})
	}
	slices.SortFunc(out.Nodes, func(x, y topoNodeJSON) int { return strings.Compare(x.Name, y.Name) })
	return nil
}

// topologyMetrics adds volume fill and server usage when VictoriaMetrics
// answers in time; the map works without them.
func (a *api) topologyMetrics(ctx context.Context, scope *projectScope, out *topologyJSON) {
	mc := a.metricsClient(ctx)
	if mc == nil {
		return
	}
	ms := metrics.Unconfined()
	if !scope.platform {
		ms = metrics.Namespaces(scope.namespaces()...)
	}
	qs := []query{
		{key: "used", expr: metrics.VolumeUsed, instant: true},
		{key: "capacity", expr: metrics.VolumeCapacity, instant: true},
	}
	if scope.platform {
		qs = append(qs,
			query{key: "nodeCPU", expr: metrics.NodeCPU, instant: true},
			query{key: "nodeCPUCapacity", expr: metrics.NodeCPUCapacity, instant: true},
			query{key: "nodeMemory", expr: metrics.NodeMemory, instant: true},
			query{key: "nodeMemoryCapacity", expr: metrics.NodeMemoryCapacity, instant: true},
		)
	}
	ctx, cancel := context.WithTimeout(ctx, topologyMetricsTimeout)
	defer cancel()
	rng, err := metrics.NewRange(a.now(), time.Hour, 0)
	if err != nil {
		return
	}
	res, err := a.runQueries(ctx, qs, rng, ms)
	if err != nil {
		a.cfg.Logger.Debug("topology: metrics left out", "err", err)
		return
	}
	out.Metrics = true
	byClaim := func(key string) map[string]float64 {
		m := map[string]float64{}
		for _, s := range res[key] {
			m[s.Labels["namespace"]+"/"+s.Labels["persistentvolumeclaim"]] = value(s)
		}
		return m
	}
	used, capacity := byClaim("used"), byClaim("capacity")
	for i := range out.Volumes {
		v := &out.Volumes[i]
		claim := v.Project + "/" + v.claim
		if u, ok := used[claim]; ok {
			v.UsedBytes = &u
		}
		if c, ok := capacity[claim]; ok {
			v.CapacityBytes = &c
		}
	}
	usage := map[string]nodeUsageJSON{}
	for _, u := range nodeUsage(res) {
		usage[u.Name] = u
	}
	for i := range out.Nodes {
		if u, ok := usage[out.Nodes[i].Name]; ok {
			out.Nodes[i].Usage = &u
		}
	}
}
