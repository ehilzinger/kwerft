package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The Kwerft upgrade preflight against a fake API (deterministic: no other
// test's nodes, pools or clusters) and a fake install repository.

type fakeReleases struct {
	releases  []upgrades.Release
	manifests map[string]*upgrades.Manifest
	calls     atomic.Int32
	err       error
}

func (f *fakeReleases) Releases(context.Context) ([]upgrades.Release, error) {
	f.calls.Add(1)
	return f.releases, f.err
}

func (f *fakeReleases) Manifest(_ context.Context, v string) (*upgrades.Manifest, error) {
	f.calls.Add(1)
	if m, ok := f.manifests[strings.TrimPrefix(v, "v")]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("manifest of %s: %w", v, upgrades.ErrNotFound)
}

func (f *fakeReleases) Notes(context.Context, string) (string, error) {
	f.calls.Add(1)
	return "Release notes.", nil
}

func testManifest(version string, supported ...string) *upgrades.Manifest {
	m := &upgrades.Manifest{Version: version, Channel: upgrades.ChannelStable,
		Image: "ghcr.io/ehilzinger/kwerft@sha256:" + strings.Repeat("c", 64)}
	m.Kubernetes.Pinned = "v1.37.2+k3s1"
	m.Kubernetes.Supported = supported
	m.Chart.Ref, m.Chart.Version = "oci://ghcr.io/ehilzinger/charts/kwerft", version
	return m
}

func testReleases() *fakeReleases {
	return &fakeReleases{
		releases: []upgrades.Release{{Version: "0.6.0", Channel: "stable"}, {Version: "0.5.1", Channel: "stable"},
			{Version: "0.5.0", Channel: "stable"}, {Version: "0.7.0-rc.1", Channel: "edge"}},
		manifests: map[string]*upgrades.Manifest{
			"0.7.0-rc.1": testManifest("0.7.0-rc.1", "1.37", "1.38"),
			"0.6.0":      testManifest("0.6.0", "1.37", "1.38"),
			"0.5.1":      testManifest("0.5.1", "1.36", "1.37"),
			"0.5.0":      testManifest("0.5.0", "1.36", "1.37"),
		},
	}
}

type fakeOCI map[string]error

func (f fakeOCI) Pullable(_ context.Context, ref string) error { return f[ref] }

type fakeSnapshotter struct{ n atomic.Int32 }

func (f *fakeSnapshotter) Snapshot(_ context.Context, path string) error {
	f.n.Add(1)
	return os.WriteFile(path, []byte("SQLite format 3"), 0o600)
}

func testNode(name string, ready bool, labels map[string]string) *corev1.Node {
	status := corev1.ConditionTrue
	if !ready {
		status = corev1.ConditionFalse
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: status}},
			NodeInfo:   corev1.NodeSystemInfo{KubeletVersion: "v1.37.1+k3s1"},
		}}
}

func testChecks(t *testing.T, objs ...client.Object) *UpgradeChecks {
	t.Helper()
	base := []client.Object{testNode("server-1", true, map[string]string{kwerftv1.LabelInstaller: "true"})}
	c := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(append(base, objs...)...).Build()
	return &UpgradeChecks{
		Reader: c, Releases: testReleases(), Registry: fakeOCI{}, Version: "0.5.0",
		Self: func(context.Context) (SelfImage, error) {
			return SelfImage{Image: "ghcr.io/ehilzinger/kwerft:0.5.0", ImageID: "ghcr.io/ehilzinger/kwerft@sha256:" + strings.Repeat("a", 64)}, nil
		},
		DataDir:   t.TempDir(),
		FreeBytes: func(string) (uint64, error) { return 50 << 30, nil },
	}
}

func checkMap(checks []kwerftv1.UpgradeCheck) map[string]kwerftv1.UpgradeCheck {
	out := map[string]kwerftv1.UpgradeCheck{}
	for _, c := range checks {
		if prev, ok := out[c.Check]; ok && !prev.Warning {
			continue // the blocking result wins over a warning of the same check
		}
		out[c.Check] = c
	}
	return out
}

func TestPreflightPasses(t *testing.T) {
	checks := testChecks(t).Kwerft(context.Background(), kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}, "")
	if b := Blocked(checks); len(b) > 0 {
		t.Fatalf("blocked: %+v", b)
	}
	m := checkMap(checks)
	for _, name := range []string{CheckTarget, CheckImagePullable, CheckNodesReady, CheckDiskSpace, CheckNoOtherOperation,
		CheckReleaseInstall, CheckInstallerNode, CheckAgentSkew, CheckDataRollback} {
		if _, ok := m[name]; !ok {
			t.Errorf("no %s check", name)
		}
	}
	if m[CheckTarget].Message != "0.5.0 → 0.6.0 (Minor)." {
		t.Errorf("target: %q", m[CheckTarget].Message)
	}
}

func TestPreflightBlocks(t *testing.T) {
	spec := kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}
	for _, c := range []struct {
		name  string
		setup func(c *UpgradeChecks)
		objs  []client.Object
		check string
		want  string
	}{
		{name: "not newer", setup: func(c *UpgradeChecks) { c.Version = "0.6.0" }, check: CheckTarget, want: "not newer"},
		{name: "unpublished", setup: func(c *UpgradeChecks) {
			c.Releases.(*fakeReleases).releases = c.Releases.(*fakeReleases).releases[1:]
		}, check: CheckTarget, want: "not a published release"},
		{name: "edge release on stable", setup: func(c *UpgradeChecks) {}, check: CheckTarget, want: "edge channel"},
		{name: "upgradeFrom", setup: func(c *UpgradeChecks) {
			c.Releases.(*fakeReleases).manifests["0.6.0"].UpgradeFrom = "0.5.1"
		}, check: CheckTarget, want: "upgrades from 0.5.1 or newer"},
		{name: "kubernetes first", setup: func(c *UpgradeChecks) {
			c.Releases.(*fakeReleases).manifests["0.6.0"].Kubernetes.Supported = []string{"1.38", "1.39"}
		}, check: CheckTarget, want: "upgrade Kubernetes first"},
		{name: "repository down", setup: func(c *UpgradeChecks) { c.Releases.(*fakeReleases).err = errors.New("HTTP 503") },
			check: CheckTarget, want: "HTTP 503"},
		{name: "private image", setup: func(c *UpgradeChecks) {
			c.Registry = fakeOCI{"oci://ghcr.io/ehilzinger/charts/kwerft:0.6.0": errors.New("chart is not pullable anonymously")}
		}, check: CheckImagePullable, want: "not pullable anonymously"},
		{name: "node not ready", objs: []client.Object{testNode("worker-1", false, nil)}, check: CheckNodesReady, want: "worker-1"},
		{name: "disk pressure", objs: []client.Object{func() *corev1.Node {
			n := testNode("server-2", true, map[string]string{kwerftv1.LabelInstaller: "true"})
			n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
			return n
		}()}, check: CheckDiskSpace, want: "short of disk space"},
		{name: "data volume", setup: func(c *UpgradeChecks) { c.FreeBytes = func(string) (uint64, error) { return 1 << 30, nil } },
			check: CheckDiskSpace, want: "1.0 GiB free"},
		{name: "another upgrade", objs: []client.Object{&kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes-v1-37-2-k3s1-abcde"},
			Spec:   kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKubernetes, Version: "v1.37.2+k3s1"},
			Status: kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeRunning}}}, check: CheckNoOtherOperation, want: "kubernetes-v1-37-2-k3s1-abcde is Running"},
		{name: "node pool", objs: []client.Object{&kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "workers"},
			Spec:   kwerftv1.NodePoolSpec{Cluster: "local", ServerType: "cx32", Location: "fsn1", Count: 2},
			Status: kwerftv1.NodePoolStatus{Nodes: []kwerftv1.PoolNode{{Name: "local-workers-abcde", Phase: PoolNodeJoining}}}}},
			check: CheckNoOtherOperation, want: "Node pool workers"},
		{name: "dev build", setup: func(c *UpgradeChecks) { c.Version = "0.1.0-dev" }, check: CheckReleaseInstall, want: "make dev-server"},
		{name: "--image install", setup: func(c *UpgradeChecks) {
			c.Self = func(context.Context) (SelfImage, error) {
				return SelfImage{Image: "ghcr.io/ehilzinger/kwerft:dev-abc123"}, nil
			}
		}, check: CheckReleaseInstall, want: "an --image install"},
		{name: "no installer node", setup: func(c *UpgradeChecks) {
			c.Reader = fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(testNode("server-1", true, nil)).Build()
		}, check: CheckInstallerNode, want: "Re-run the installer"},
		{name: "agent behind", objs: []client.Object{&kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge"},
			Spec:   kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted},
			Status: kwerftv1.ClusterStatus{Phase: ClusterConnected, AgentVersion: "0.4.2"}}}, check: CheckAgentSkew, want: "edge runs 0.4.2"},
		{name: "data rollback", setup: func(c *UpgradeChecks) {
			c.Releases.(*fakeReleases).manifests["0.6.0"].RollbackSafe = ptr.To(false)
		}, check: CheckDataRollback, want: "not rollback-safe"},
	} {
		t.Run(c.name, func(t *testing.T) {
			checks := testChecks(t, c.objs...)
			if c.setup != nil {
				c.setup(checks)
			}
			s := spec
			if c.name == "edge release on stable" {
				s.Version = "0.7.0-rc.1"
			}
			got := checkMap(checks.Kwerft(context.Background(), s, ""))[c.check]
			if got.OK || got.Warning || !strings.Contains(got.Message, c.want) {
				t.Errorf("%s = %+v, want a failure with %q", c.check, got, c.want)
			}
		})
	}
}

func TestPreflightDataRollbackAccepted(t *testing.T) {
	c := testChecks(t)
	c.Releases.(*fakeReleases).manifests["0.6.0"].RollbackSafe = ptr.To(false)
	checks := c.Kwerft(context.Background(), kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0", AcceptDataRollback: true}, "")
	if b := Blocked(checks); len(b) > 0 {
		t.Fatalf("blocked: %+v", b)
	}
}

func TestPreflightWarnsAboutDisconnectedAgents(t *testing.T) {
	c := testChecks(t, &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "away"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted},
		Status: kwerftv1.ClusterStatus{Phase: ClusterDisconnected, AgentVersion: "0.3.0"}})
	checks := c.Kwerft(context.Background(), kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}, "")
	if b := Blocked(checks); len(b) > 0 {
		t.Fatalf("blocked: %+v", b)
	}
	var warned bool
	for _, ch := range checks {
		warned = warned || (ch.Warning && strings.Contains(ch.Message, "away"))
	}
	if !warned {
		t.Errorf("no warning about the disconnected cluster: %+v", checks)
	}
}

func TestGenerateName(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"Kwerft", "0.6.0"}:            "kwerft-0.6.0-",
		{"Kubernetes", "v1.38.1+k3s1"}: "kubernetes-v1.38.1-k3s1-",
	} {
		if got := GenerateName(kwerftv1.UpgradeComponent(in[0]), in[1]); got != want {
			t.Errorf("%v: %s, want %s", in, got, want)
		}
	}
}
