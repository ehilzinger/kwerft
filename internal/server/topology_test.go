// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ehilzinger/kwerft/internal/controllers"
)

type topoPodResp struct {
	Name   string `json:"name"`
	Node   string `json:"node"`
	Status string `json:"status"`
	Tone   string `json:"tone"`
}

type topoAppResp struct {
	Name    string          `json:"name"`
	Project string          `json:"project"`
	Mounts  []topoMountJSON `json:"mounts"`
	Pods    []topoPodResp   `json:"pods"`
}

type topoProjectResp struct {
	Name     string `json:"name"`
	Isolated bool   `json:"isolated"`
	Placed   bool   `json:"placed"`
}

type topoVolumeResp struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Own     bool   `json:"own"`
	App     string `json:"app"`
	Node    string `json:"node"`
}

type topologyResp struct {
	Cluster      string             `json:"cluster"`
	Projects     []topoProjectResp  `json:"projects"`
	Apps         []topoAppResp      `json:"apps"`
	Schedules    []listed           `json:"schedules"`
	Tasks        []listed           `json:"tasks"`
	Volumes      []topoVolumeResp   `json:"volumes"`
	Domains      []listed           `json:"domains"`
	Rules        []listed           `json:"rules"`
	Nodes        []listed           `json:"nodes"`
	NodesPartial bool               `json:"nodesPartial"`
	Firewall     []firewallRuleJSON `json:"firewall"`
	Metrics      bool               `json:"metrics"`
}

// placedPod is a running pod of app on node, mounting claim when set.
func placedPod(t *testing.T, project, app, name, node, claim string) {
	t.Helper()
	ctx := context.Background()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project, Labels: map[string]string{controllers.LabelApp: app, controllers.LabelProject: project}},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "app", Image: "nginx:1.27"}}},
	}
	if claim != "" {
		pod.Spec.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim}}}}
	}
	if err := cluster.admin.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "app", Ready: true, Image: "nginx:1.27", ImageID: "nginx@sha256:0",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()}},
	}}}
	if err := cluster.admin.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func TestTopology(t *testing.T) {
	c := newConsole(t)
	const project, node = "topo-shop", "topo-node-1"
	c.project(t, project)
	ctx := context.Background()
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node, Labels: map[string]string{"kwerft.dev/platform": "dedicated"}}}
	if err := cluster.admin.Create(ctx, n); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cluster.admin.Delete(ctx, n) })

	// A shared volume on api, an own disk on db, a rule between web and api.
	api := imageApp("api", "nginx:1.27")
	api["spec"].(map[string]any)["volumes"] = []map[string]any{{"path": "/data", "volume": "data"}}
	db := imageApp("db", "postgres:17")
	db["spec"].(map[string]any)["volumes"] = []map[string]any{{"path": "/var/lib/postgresql/data", "size": "1Gi"}}
	for _, req := range []struct {
		path string
		body any
	}{
		{"/volumes", map[string]string{"name": "data", "size": "1Gi"}},
		{"/apps", imageApp("web", "nginx:1.27")},
		{"/apps", api},
		{"/apps", db},
		{"/trafficrules", map[string]any{"name": "web-to-api", "from": []map[string]any{{"app": "web"}},
			"to": []map[string]any{{"app": "api"}}, "ports": []map[string]any{{"port": 8080}}}},
		{"/schedules", scheduleBody("nightly", "0 2 * * *")},
	} {
		if code := c.dev.do(t, "POST", "/api/v1/projects/"+project+req.path, req.body, nil); code != http.StatusCreated {
			t.Fatalf("create %s: %d", req.path, code)
		}
	}
	placedPod(t, project, "api", "api-0", node, "data")
	placedPod(t, project, "db", "db-0", node, "data-0-db-0")

	var owner topologyResp
	eventually(t, func() error {
		owner = topologyResp{}
		if code := c.owner.do(t, "GET", "/api/v1/topology", nil, &owner); code != http.StatusOK {
			return fmt.Errorf("owner: %d", code)
		}
		if len(owner.Apps) < 3 || len(owner.Schedules) < 1 || len(owner.Rules) < 1 {
			return fmt.Errorf("not all listed yet: %d apps, %d schedules, %d rules", len(owner.Apps), len(owner.Schedules), len(owner.Rules))
		}
		return nil
	})

	app := func(r topologyResp, name string) (int, bool) {
		i := slices.IndexFunc(r.Apps, func(a topoAppResp) bool { return a.Project == project && a.Name == name })
		return i, i >= 0
	}
	i, ok := app(owner, "api")
	if !ok {
		t.Fatalf("apps = %+v, want api", owner.Apps)
	}
	if a := owner.Apps[i]; len(a.Pods) != 1 || a.Pods[0].Node != node || a.Pods[0].Tone != "ok" ||
		len(a.Mounts) != 1 || a.Mounts[0].Volume != "data" {
		t.Errorf("api = %+v, want one pod on %s mounting data", a, node)
	}
	i, _ = app(owner, "db")
	if a := owner.Apps[i]; len(a.Mounts) != 1 || a.Mounts[0].Volume != "db/data-0" {
		t.Errorf("db mounts = %+v, want its own disk db/data-0", a.Mounts)
	}
	volumes := map[string]bool{}
	for _, v := range owner.Volumes {
		if v.Project != project {
			continue
		}
		volumes[v.Name] = true
		if v.Node != node {
			t.Errorf("volume %s on %q, want %s (the node of the pod mounting it)", v.Name, v.Node, node)
		}
		if v.Own != (v.Name == "db/data-0") || (v.Own && v.App != "db") {
			t.Errorf("volume %+v: own disk wrong", v)
		}
	}
	if !volumes["data"] || !volumes["db/data-0"] {
		t.Errorf("volumes = %v, want data and db/data-0", volumes)
	}
	if !slices.Contains(owner.Rules, listed{Name: "web-to-api", Project: project}) {
		t.Errorf("rules = %+v", owner.Rules)
	}
	if owner.NodesPartial || !slices.Contains(owner.Nodes, listed{Name: node}) {
		t.Errorf("owner nodes = %+v (partial %v), want every server", owner.Nodes, owner.NodesPartial)
	}
	if owner.Firewall == nil {
		t.Error("owner: firewall is null, want the rules (an empty list without any)")
	}
	if owner.Cluster != "local" || owner.Metrics {
		t.Errorf("cluster %q, metrics %v: want local, without metrics", owner.Cluster, owner.Metrics)
	}

	// A viewer: the same project, but servers only by name, no firewall.
	var viewer topologyResp
	if code := c.viewer.do(t, "GET", "/api/v1/topology", nil, &viewer); code != http.StatusOK {
		t.Fatalf("viewer: %d", code)
	}
	if viewer.Firewall != nil {
		t.Errorf("viewer sees the firewall: %+v", viewer.Firewall)
	}
	podNodes := map[string]bool{}
	for _, a := range viewer.Apps {
		for _, p := range a.Pods {
			podNodes[p.Node] = true
		}
	}
	if !viewer.NodesPartial || !slices.Contains(viewer.Nodes, listed{Name: node}) ||
		slices.ContainsFunc(viewer.Nodes, func(n listed) bool { return !podNodes[n.Name] }) {
		t.Errorf("viewer nodes = %+v (partial %v), want only the servers of their pods", viewer.Nodes, viewer.NodesPartial)
	}
	pi := slices.IndexFunc(viewer.Projects, func(p topoProjectResp) bool { return p.Name == project })
	if pi < 0 || !viewer.Projects[pi].Placed || !viewer.Projects[pi].Isolated {
		t.Errorf("viewer projects = %+v, want %s placed (viewers read pods) and isolated", viewer.Projects, project)
	}

	// A cluster that does not exist.
	if code := c.owner.do(t, "GET", "/api/v1/topology?cluster=nowhere", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown cluster: %d, want 404", code)
	}
}
