package hetzner_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

func nodesFake(t *testing.T) (*hetznertest.Server, *hetzner.Client) {
	t.Helper()
	f := hetznertest.New(t, "tok")
	f.FakeNodes()
	return f, f.Client()
}

func TestCatalog(t *testing.T) {
	_, c := nodesFake(t)
	ctx := context.Background()
	locs, err := c.Locations(ctx)
	if err != nil || len(locs) != 4 || locs[0].NetworkZone != "eu-central" {
		t.Fatalf("locations = %+v, %v", locs, err)
	}
	types, err := c.ServerTypes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cx23, ok := hetzner.FindServerType(types, "cx23")
	if !ok || !cx23.AvailableIn("fsn1") || cx23.AvailableIn("sin") || cx23.MonthlyGross("nbg1") == "" {
		t.Fatalf("cx23 = %+v", cx23)
	}
	imgs, err := c.SystemImages(ctx, "arm")
	if err != nil {
		t.Fatal(err)
	}
	img, ok := hetzner.NewestUbuntu(imgs, "arm")
	if !ok || img.Name != "ubuntu-26.04" || img.Architecture != "arm" {
		t.Fatalf("newest = %+v", img)
	}
}

func TestNewestUbuntuFallsBack(t *testing.T) {
	dep := "2026-01-01T00:00:00Z"
	imgs := []hetzner.Image{
		{Type: "system", Status: "available", Name: "ubuntu-26.04", Architecture: "x86", Deprecated: &dep},
		{Type: "system", Status: "available", Name: "ubuntu-24.04", Architecture: "x86"},
		{Type: "system", Status: "available", Name: "debian-13", Architecture: "x86"},
	}
	if img, ok := hetzner.NewestUbuntu(imgs, "x86"); !ok || img.Name != "ubuntu-24.04" {
		t.Fatalf("got %+v", img)
	}
	if _, ok := hetzner.NewestUbuntu(imgs, "arm"); ok {
		t.Fatal("no arm image should be found")
	}
}

func TestServerLifecycle(t *testing.T) {
	f, c := nodesFake(t)
	ids := f.FakeClusterNetworks(hetzner.NetworkRef{Name: "kwerft-prod", IPRange: "10.0.0.0/16"})
	ctx := context.Background()

	net, err := c.ClusterNetwork(ctx, "prod")
	if err != nil || net.ID != ids[0] {
		t.Fatalf("network = %+v, %v", net, err)
	}
	if _, err := c.ClusterNetwork(ctx, "other"); !errors.Is(err, hetzner.ErrNotFound) {
		t.Fatalf("other network: %v", err)
	}
	f.AddSSHKey("ops", nil)
	marked := f.AddSSHKey("kwerft", map[string]string{hetzner.LabelSSHKey: ""})
	keys, err := c.ServerSSHKeys(ctx)
	if err != nil || len(keys) != 1 || keys[0] != strconv.FormatInt(marked, 10) {
		t.Fatalf("keys = %v, %v (want only the marked one)", keys, err)
	}

	pg, err := c.CreatePlacementGroup(ctx, hetzner.PlacementGroupName("prod", "workers"), map[string]string{hetzner.LabelCluster: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{hetzner.LabelCluster: "prod", hetzner.LabelPool: "workers"}
	srv, action, err := c.CreateServer(ctx, hetzner.CreateServerOpts{
		Name: "prod-workers-abcde", ServerType: "cx23", Location: "fsn1", Image: "ubuntu-26.04",
		PlacementGroup: pg.ID, SSHKeys: keys, Networks: []int64{net.ID}, UserData: "#cloud-config\n", Labels: labels,
	})
	if err != nil || action.Err() != nil {
		t.Fatalf("create: %v %v", err, action.Err())
	}
	if srv.PrivateIP(net.ID) == "" || srv.PublicIPv4() == "" || srv.PlacementGroup == nil {
		t.Fatalf("server = %+v", srv)
	}
	if got, err := c.Action(ctx, action.ID); err != nil || got.Status != hetzner.ActionSuccess {
		t.Fatalf("action = %+v, %v", got, err)
	}
	list, err := c.Servers(ctx, hetzner.LabelCluster+"=prod,"+hetzner.LabelPool+"=workers")
	if err != nil || len(list) != 1 || list[0].Name != srv.Name {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if list, _ := c.Servers(ctx, hetzner.LabelPool+"=other"); len(list) != 0 {
		t.Fatalf("selector ignored: %+v", list)
	}
	if f.UserData(srv.ID) != "#cloud-config\n" {
		t.Fatal("user data not kept")
	}
	// Unavailable types fail like the real API.
	f.SetTypeAvailable("cx33", "nbg1", false)
	_, _, err = c.CreateServer(ctx, hetzner.CreateServerOpts{Name: "x", ServerType: "cx33", Location: "nbg1", Image: "ubuntu-26.04"})
	var apiErr *hetzner.APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "resource_unavailable" {
		t.Fatalf("unavailable type: %v", err)
	}
	if _, _, err := c.CreateServer(ctx, hetzner.CreateServerOpts{Name: "y", ServerType: "cx23", Location: "fsn1", Image: "x", UserData: strings.Repeat("a", hetzner.MaxUserData+1)}); err == nil {
		t.Fatal("oversized user data accepted")
	}

	if err := c.DeleteServer(ctx, srv.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteServer(ctx, srv.ID); err != nil {
		t.Fatalf("deleting a deleted server: %v", err)
	}
	if _, err := c.Server(ctx, srv.ID); !errors.Is(err, hetzner.ErrNotFound) {
		t.Fatalf("get deleted: %v", err)
	}
	if err := c.DeletePlacementGroup(ctx, pg.ID); err != nil {
		t.Fatal(err)
	}
}

func TestVSwitchCoupling(t *testing.T) {
	robot := hetznertest.NewRobot(t, "robot-user", "robot-pass")
	rc := robot.Client()
	ctx := context.Background()
	vs, err := rc.CreateVSwitch(ctx, "kwerft-prod", 4000)
	if err != nil || vs.ID == 0 {
		t.Fatalf("create: %+v %v", vs, err)
	}
	if err := rc.AddVSwitchServers(ctx, vs.ID, []string{"198.51.100.7", "321"}); err != nil {
		t.Fatal(err)
	}
	got, err := rc.VSwitch(ctx, vs.ID)
	if err != nil || len(got.Server) != 2 || got.VLAN != 4000 {
		t.Fatalf("vswitch = %+v, %v", got, err)
	}
	if _, err := rc.CreateVSwitch(ctx, "again", 4000); err == nil {
		t.Fatal("a second vSwitch on the same VLAN was accepted")
	}
	if _, err := rc.VSwitch(ctx, 999); !errors.Is(err, hetzner.ErrNotFound) {
		t.Fatalf("missing vswitch: %v", err)
	}
	bad := &hetzner.RobotClient{User: "robot-user", Password: "wrong", Base: robot.URL}
	if _, err := bad.VSwitches(ctx); !errors.Is(err, hetzner.ErrTokenRejected) {
		t.Fatalf("bad credentials: %v", err)
	}

	f, c := nodesFake(t)
	ids := f.FakeClusterNetworks(hetzner.NetworkRef{Name: "kwerft-prod", IPRange: "10.0.0.0/16"})
	if _, err := c.AddVSwitchSubnet(ctx, ids[0], "10.0.64.0/24", "eu-central", vs.ID); err != nil {
		t.Fatal(err)
	}
	if subs := f.VSwitchSubnets(); len(subs) != 1 || !strings.Contains(subs[0], "/10.0.64.0/24/eu-central") {
		t.Fatalf("subnets = %v", subs)
	}
}

func TestRobotRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"status":403,"code":"RATE_LIMIT_EXCEEDED","message":"rate limit exceeded"}}`))
	}))
	defer srv.Close()
	c := &hetzner.RobotClient{User: "u", Password: "p", Base: srv.URL}
	var rl *hetzner.RateLimitError
	if _, err := c.VSwitches(context.Background()); !errors.As(err, &rl) {
		t.Fatalf("err = %v", err)
	}
}
