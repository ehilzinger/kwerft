// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/store"
)

type clusterView struct {
	Name         string `json:"name"`
	Provider     string `json:"provider"`
	Phase        string `json:"phase"`
	Connected    bool   `json:"connected"`
	HasToken     bool   `json:"hasToken"`
	HetznerCloud *struct {
		Location      string `json:"location"`
		ServerType    string `json:"serverType"`
		ControlPlanes int32  `json:"controlPlanes"`
	} `json:"hetznerCloud"`
	Agent *struct {
		Remote string `json:"remote"`
	} `json:"agent"`
}

type adoptResponse struct {
	Cluster        clusterView `json:"cluster"`
	Token          string      `json:"token"`
	InstallCommand string      `json:"installCommand"`
}

func withTunnel(hub *clusters.Hub) func(*Config) {
	return func(c *Config) {
		c.Tunnel, c.Clusters = hub, hub
		c.ConsoleDomain = "console.example.com"
	}
}

func TestClustersAPI(t *testing.T) {
	requireCluster(t)
	hub := &clusters.Hub{Local: cluster.console, Clusters: cluster.admin, Logger: slog.New(slog.DiscardHandler)}
	c := newConsole(t, withTunnel(hub))
	ctx := context.Background()

	// Owners and admins only.
	for _, s := range []*session{c.dev, c.viewer} {
		if code := s.do(t, "GET", "/api/v1/clusters", nil, nil); code != http.StatusForbidden {
			t.Errorf("non-admin lists clusters: %d", code)
		}
		if code := s.do(t, "POST", "/api/v1/clusters", map[string]any{"name": "nope", "provider": "adopted"}, nil); code != http.StatusForbidden {
			t.Errorf("non-admin creates a cluster: %d", code)
		}
	}

	// Names and providers are checked before Kubernetes sees them.
	for _, bad := range []map[string]any{
		{"name": "Edge", "provider": "adopted"},
		{"name": "local", "provider": "adopted"},
		{"name": "connect", "provider": "adopted"},
		{"name": strings.Repeat("a", 41), "provider": "adopted"},
		{"name": "edge-x", "provider": "aws"},
		{"name": "edge-x", "provider": "hetzner-cloud"},
		{"name": "edge-x", "provider": "hetzner-cloud", "hetznerCloud": map[string]any{"location": "Falkenstein", "serverType": "cx32"}},
		{"name": "edge-x", "provider": "hetzner-cloud", "hetznerCloud": map[string]any{"location": "fsn1", "serverType": "cx32", "controlPlanes": 2}},
	} {
		var e apiError
		if code := c.owner.do(t, "POST", "/api/v1/clusters", bad, &e); code != http.StatusBadRequest || e.Field == "" {
			t.Errorf("create %v: %d %+v, want 400 with a field", bad, code, e)
		}
	}

	// Adopt: the token is shown once, in the install command; only its
	// hash is kept, on the Cluster.
	var adopted adoptResponse
	if code := c.owner.do(t, "POST", "/api/v1/clusters", map[string]any{"name": "api-edge", "displayName": "Edge", "provider": "adopted"}, &adopted); code != http.StatusCreated {
		t.Fatalf("adopt: %d", code)
	}
	if name, ok := clusters.AgentTokenCluster(adopted.Token); !ok || name != "api-edge" {
		t.Errorf("token %q", adopted.Token)
	}
	wantCmd := "| sudo bash -s -- --agent --console https://console.example.com --cluster-token " + adopted.Token
	if !strings.HasPrefix(adopted.InstallCommand, "curl -fsSL https://") || !strings.Contains(adopted.InstallCommand, wantCmd) {
		t.Errorf("install command %q", adopted.InstallCommand)
	}
	var cl kwerftv1.Cluster
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "api-edge"}, &cl); err != nil {
		t.Fatal(err)
	}
	if cl.Spec.Provider != kwerftv1.ClusterAdopted || !auth.TokenMatches(adopted.Token, cl.Annotations[clusters.TokenHashAnnotation]) ||
		strings.Contains(fmt.Sprint(cl.Annotations, cl.Spec), adopted.Token) {
		t.Errorf("stored cluster %+v", cl)
	}
	if code := c.owner.do(t, "POST", "/api/v1/clusters", map[string]any{"name": "api-edge", "provider": "adopted"}, nil); code != http.StatusConflict {
		t.Errorf("adopt twice: %d", code)
	}

	// Create on Hetzner Cloud: no token in the answer (the reconciler keeps it for cloud-init).
	var cloud adoptResponse
	if code := c.owner.do(t, "POST", "/api/v1/clusters", map[string]any{"name": "api-cloud", "provider": "hetzner-cloud",
		"hetznerCloud": map[string]any{"location": "nbg1", "serverType": "cx32", "controlPlanes": 3}}, &cloud); code != http.StatusCreated {
		t.Fatalf("create on Hetzner Cloud: %d", code)
	}
	if cloud.Token != "" || cloud.InstallCommand != "" || cloud.Cluster.HetznerCloud == nil || cloud.Cluster.HetznerCloud.ControlPlanes != 3 {
		t.Errorf("hetzner-cloud create answered %+v", cloud)
	}

	// The agent connects with the token: the list says so.
	agentCtx, stopAgent := context.WithCancel(ctx)
	defer stopAgent()
	agent := &clusters.Agent{ConsoleURL: c.owner.url, Token: func() (string, error) { return adopted.Token, nil }, API: cluster.console,
		Logger: slog.New(slog.DiscardHandler), MinBackoff: 20 * time.Millisecond, MaxBackoff: 100 * time.Millisecond, Rejected: 100 * time.Millisecond}
	go func() { _ = agent.Run(agentCtx) }()
	eventually(t, func() error {
		if _, ok := hub.Agent("api-edge"); !ok {
			return errNotYet
		}
		return nil
	})
	var list struct {
		Clusters []clusterView `json:"clusters"`
	}
	if code := c.owner.do(t, "GET", "/api/v1/clusters", nil, &list); code != http.StatusOK {
		t.Fatalf("list: %d", code)
	}
	byName := map[string]clusterView{}
	for _, v := range list.Clusters {
		byName[v.Name] = v
	}
	if v := byName["api-edge"]; !v.Connected || v.Phase != "Connected" || v.Agent == nil || !v.HasToken {
		t.Errorf("connected cluster in the list: %+v", v)
	}
	if v := byName["api-cloud"]; v.Connected || v.Provider != "hetzner-cloud" {
		t.Errorf("cloud cluster in the list: %+v", v)
	}
	var one clusterView
	if code := c.owner.do(t, "GET", "/api/v1/clusters/api-edge", nil, &one); code != http.StatusOK || !one.Connected {
		t.Errorf("get: %d %+v", code, one)
	}

	// Rotating disconnects the agent; its old token is refused from then on.
	var rotated adoptResponse
	if code := c.owner.do(t, "POST", "/api/v1/clusters/api-edge/token", nil, &rotated); code != http.StatusOK {
		t.Fatalf("rotate: %d", code)
	}
	if rotated.Token == adopted.Token || !strings.Contains(rotated.InstallCommand, rotated.Token) {
		t.Errorf("rotate answered %+v", rotated)
	}
	if _, ok := hub.Agent("api-edge"); ok {
		t.Error("agent still connected after rotation")
	}
	time.Sleep(300 * time.Millisecond) // the agent tries again, with the old token
	if _, ok := hub.Agent("api-edge"); ok {
		t.Error("agent reconnected with the rotated-out token")
	}
	page, err := c.store.QueryAudit(ctx, store.AuditQuery{Action: "cluster.agent_rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) == 0 || page.Entries[0].Target != "api-edge" {
		t.Errorf("refused agents are not audited: %+v", page.Entries)
	}

	// The management cluster stays.
	for _, path := range []string{"/api/v1/clusters/local", "/api/v1/clusters/local/token"} {
		method := map[bool]string{true: "DELETE", false: "POST"}[!strings.HasSuffix(path, "/token")]
		if code := c.owner.do(t, method, path, nil, nil); code != http.StatusBadRequest {
			t.Errorf("%s %s: %d, want 400", method, path, code)
		}
	}

	// Delete (as the owner, impersonated).
	if code := c.owner.do(t, "DELETE", "/api/v1/clusters/api-edge", nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if err := cluster.admin.Get(ctx, client.ObjectKey{Name: "api-edge"}, &cl); !apierrors.IsNotFound(err) {
		t.Errorf("cluster still there: %v", err)
	}
	if code := c.owner.do(t, "DELETE", "/api/v1/clusters/api-edge", nil, nil); code != http.StatusNotFound {
		t.Errorf("delete twice: %d", code)
	}
	_ = cluster.admin.Delete(ctx, &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "api-cloud"}})
}

func TestClusterConnectIsForAgentsOnly(t *testing.T) {
	requireCluster(t)
	hub := &clusters.Hub{Local: cluster.console, Clusters: cluster.admin, Logger: slog.New(slog.DiscardHandler)}
	c := newConsole(t, withTunnel(hub))
	call := func(header map[string]string) int {
		req, _ := http.NewRequest("GET", c.owner.url+clusters.ConnectPath, nil)
		for k, v := range header {
			req.Header.Set(k, v)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		return res.StatusCode
	}
	// A signed-in browser gets nowhere: the session is no credential here.
	if code := c.owner.do(t, "GET", clusters.ConnectPath, nil, nil); code != http.StatusUnauthorized {
		t.Errorf("session on the agent endpoint: %d", code)
	}
	if code := call(map[string]string{"Authorization": "Bearer " + clusters.NewAgentToken("nowhere"), "Origin": "https://evil.example"}); code != http.StatusForbidden {
		t.Errorf("with an Origin: %d", code)
	}
	// Refused attempts are limited per client.
	limited := false
	for range 25 {
		if code := call(map[string]string{"Authorization": "Bearer " + clusters.NewAgentToken("nowhere")}); code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("no rate limit on refused agents")
	}
}

type notYet struct{}

func (notYet) Error() string { return "not yet" }

var errNotYet error = notYet{}

// The install command fetches the console's own release of the installer:
// the latest stable one may predate --agent (a release candidate's console).
func TestAgentInstallCommand(t *testing.T) {
	const tail = " | sudo bash -s -- --agent --console https://ops.example.com --cluster-token kwft_agent_x"
	for _, tc := range []struct{ version, want string }{
		{"0.4.0", "curl -fsSL https://kwerft.dev/v0.4.0/install.sh" + tail + " --version 0.4.0"},
		{"v0.5.0-rc.2", "curl -fsSL https://kwerft.dev/v0.5.0-rc.2/install.sh" + tail + " --version 0.5.0-rc.2"},
		{"0.1.0-dev", "curl -fsSL https://kwerft.dev/install.sh" + tail},
		{"dev-abc123", "curl -fsSL https://kwerft.dev/install.sh" + tail},
	} {
		if got := agentInstallCommand("ops.example.com", "kwft_agent_x", tc.version); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.version, got, tc.want)
		}
	}
}
