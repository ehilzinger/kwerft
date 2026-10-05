package controllers

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/upgrades"
)

// The Kubernetes upgrade preflight against a fake API.

func k3sTestNode(name, version string, server, ready bool) *corev1.Node {
	n := testNode(name, ready, map[string]string{})
	n.Status.NodeInfo.KubeletVersion = version
	if server {
		n.Labels[upgrades.LabelControlPlane] = "true"
		n.Labels[upgrades.LabelEtcd] = "true"
	}
	return n
}

func sucObjects(ready bool) []client.Object {
	crd := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "plans.upgrade.cattle.io"},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: upgrades.SUCNamespace, Name: upgrades.SUCDeployment},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1)}}
	if ready {
		d.Status = appsv1.DeploymentStatus{UpdatedReplicas: 1, AvailableReplicas: 1}
	}
	return []client.Object{crd, d}
}

// k3sChecks: the installer's server plus objs; three servers by default.
func k3sChecks(t *testing.T, api *fakeAPIServer, objs ...client.Object) *UpgradeChecks {
	t.Helper()
	scheme := NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if objs == nil {
		objs = append(sucObjects(true),
			k3sTestNode("server-2", "v1.37.1+k3s1", true, true), k3sTestNode("server-3", "v1.37.1+k3s1", true, true),
			k3sTestNode("worker-1", "v1.37.1+k3s1", false, true))
	}
	installer := k3sTestNode("server-1", "v1.37.1+k3s1", true, true)
	installer.Labels[kwerftv1.LabelInstaller] = "true"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objs, installer)...).Build()
	return &UpgradeChecks{Reader: c, Releases: testReleases(), Registry: fakeOCI{}, Version: "0.5.0", APIServer: api}
}

func k3sSpec(v string) kwerftv1.UpgradeSpec {
	return kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKubernetes, Version: v}
}

func TestK3sPreflightPasses(t *testing.T) {
	checks := k3sChecks(t, &fakeAPIServer{}).Kubernetes(context.Background(), k3sSpec("v1.37.2+k3s1"), "")
	m := checkMap(checks)
	for _, name := range []string{CheckTarget, CheckNodesReady, CheckNodesSameVersion, CheckEtcd, CheckDeprecatedAPIs, CheckDiskSpace,
		CheckNoOtherOperation, CheckKwerftSupports, CheckUpgradeController, CheckInstallerNode} {
		if c, ok := m[name]; !ok || !c.OK {
			t.Errorf("%s: %+v", name, c)
		}
	}
	if !strings.Contains(m[CheckTarget].Message, "(Patch)") || !strings.Contains(m[CheckEtcd].Message, "3 members") {
		t.Errorf("target %q, etcd %q", m[CheckTarget].Message, m[CheckEtcd].Message)
	}
}

func TestK3sPreflightTargets(t *testing.T) {
	cases := map[string]string{
		"0.6.0":            "not a k3s version",
		"v1.37.1+k3s1":     "already runs",
		"v1.36.9+k3s1":     "cannot be downgraded",
		"v1.39.0+k3s1":     "from 1.37 to 1.38 first",
		"v1.37.1+k3s2":     "", // a k3s build counts as newer
		"v1.38.1+k3s1":     "", // allowed as a target, but see KwerftSupports
		"v1.37.2-rc1+k3s1": "",
	}
	for v, want := range cases {
		c := checkMap(k3sChecks(t, &fakeAPIServer{}).Kubernetes(context.Background(), k3sSpec(v), ""))[CheckTarget]
		if want == "" && !c.OK || want != "" && (c.OK || !strings.Contains(c.Message, want)) {
			t.Errorf("%s: %+v, want %q", v, c, want)
		}
	}
	// 0.5.0 supports 1.36 and 1.37 only.
	c := checkMap(k3sChecks(t, &fakeAPIServer{}).Kubernetes(context.Background(), k3sSpec("v1.38.1+k3s1"), ""))[CheckKwerftSupports]
	if c.OK || !strings.Contains(c.Message, "upgrade Kwerft first") {
		t.Errorf("KwerftSupports = %+v", c)
	}
	checks := k3sChecks(t, &fakeAPIServer{})
	checks.Version = "0.6.0-dev"
	if c := checkMap(checks.Kubernetes(context.Background(), k3sSpec("v1.37.2+k3s1"), ""))[CheckKwerftSupports]; c.OK || !c.Warning {
		t.Errorf("dev build: %+v", c)
	}
}

func TestK3sPreflightEtcd(t *testing.T) {
	ctx := context.Background()
	spec := k3sSpec("v1.37.2+k3s1")
	if c := checkMap(k3sChecks(t, &fakeAPIServer{etcdErr: errors.New("[-]etcd failed: reason withheld")}).Kubernetes(ctx, spec, ""))[CheckEtcd]; c.OK {
		t.Errorf("unhealthy etcd passes: %+v", c)
	}
	// Two of three servers down: no quorum.
	down := append(sucObjects(true), k3sTestNode("server-2", "v1.37.1+k3s1", true, false), k3sTestNode("server-3", "v1.37.1+k3s1", true, false))
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, down...).Kubernetes(ctx, spec, ""))[CheckEtcd]; c.OK || !strings.Contains(c.Message, "lost quorum") {
		t.Errorf("quorum lost: %+v", c)
	}
	// Two servers: passes with a warning.
	two := append(sucObjects(true), k3sTestNode("server-2", "v1.37.1+k3s1", true, true))
	checks := k3sChecks(t, &fakeAPIServer{}, two...).Kubernetes(ctx, spec, "")
	var warned bool
	for _, c := range checks {
		warned = warned || (c.Check == CheckEtcd && c.Warning && strings.Contains(c.Message, "quorum is lost"))
	}
	if !warned || len(Blocked(checks)) > 0 {
		t.Errorf("two servers: %+v", checks)
	}
	// One server.
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, sucObjects(true)...).Kubernetes(ctx, spec, ""))[CheckEtcd]; !c.OK || !strings.Contains(c.Message, "one server") {
		t.Errorf("one server: %+v", c)
	}
}

func TestK3sPreflightDeprecatedAPIs(t *testing.T) {
	ctx := context.Background()
	api := &fakeAPIServer{apis: []upgrades.DeprecatedAPI{
		{Group: "flowcontrol.apiserver.k8s.io", Version: "v1beta3", Resource: "flowschemas", RemovedRelease: "1.37"},
		{Group: "policy", Version: "v1beta1", Resource: "poddisruptionbudgets", RemovedRelease: "1.40"},
	}}
	// A minor (1.36 → 1.37) is blocked.
	older := append(sucObjects(true), k3sTestNode("server-2", "v1.36.4+k3s1", true, true))
	checks := k3sChecks(t, api, older...)
	// The installer's node runs 1.36 too.
	var node corev1.Node
	_ = checks.Reader.Get(ctx, client.ObjectKey{Name: "server-1"}, &node)
	node.Status.NodeInfo.KubeletVersion = "v1.36.4+k3s1"
	if err := checks.Reader.(client.Client).Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	c := checkMap(checks.Kubernetes(ctx, k3sSpec("v1.37.2+k3s1"), ""))[CheckDeprecatedAPIs]
	if c.OK || c.Warning || !strings.Contains(c.Message, "flowcontrol.apiserver.k8s.io/v1beta3 flowschemas (removed in 1.37)") || strings.Contains(c.Message, "poddisruptionbudgets") {
		t.Errorf("minor: %+v", c)
	}
	// A patch only warns.
	c = checkMap(k3sChecks(t, api).Kubernetes(ctx, k3sSpec("v1.37.2+k3s1"), ""))[CheckDeprecatedAPIs]
	if c.OK || !c.Warning {
		t.Errorf("patch: %+v", c)
	}
	// Unreadable metrics: a minor is blocked.
	checks.APIServer = &fakeAPIServer{err: errors.New("forbidden")}
	if c := checkMap(checks.Kubernetes(ctx, k3sSpec("v1.37.2+k3s1"), ""))[CheckDeprecatedAPIs]; c.OK || c.Warning {
		t.Errorf("no metrics, minor: %+v", c)
	}
}

func TestK3sPreflightBlocks(t *testing.T) {
	ctx := context.Background()
	spec := k3sSpec("v1.37.2+k3s1")
	mixed := append(sucObjects(true), k3sTestNode("worker-1", "v1.36.4+k3s1", false, true))
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, mixed...).Kubernetes(ctx, spec, ""))[CheckNodesSameVersion]; c.OK || !strings.Contains(c.Message, "v1.36.4+k3s1: worker-1") {
		t.Errorf("mixed versions: %+v", c)
	}
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, sucObjects(false)...).Kubernetes(ctx, spec, ""))[CheckUpgradeController]; c.OK || !strings.Contains(c.Message, "not ready") {
		t.Errorf("SUC not ready: %+v", c)
	}
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, k3sTestNode("worker-1", "v1.37.1+k3s1", false, true)).Kubernetes(ctx, spec, ""))[CheckUpgradeController]; c.OK || !strings.Contains(c.Message, "Re-run the installer") {
		t.Errorf("no SUC: %+v", c)
	}
	full := k3sTestNode("worker-1", "v1.37.1+k3s1", false, true)
	full.Status.Conditions = append(full.Status.Conditions, corev1.NodeCondition{Type: corev1.NodeDiskPressure, Status: corev1.ConditionTrue})
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, append(sucObjects(true), full)...).Kubernetes(ctx, spec, ""))[CheckDiskSpace]; c.OK || !strings.Contains(c.Message, "worker-1") {
		t.Errorf("disk pressure: %+v", c)
	}
	busy := &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: "kwerft-0.6.0-x"}, Status: kwerftv1.UpgradeStatus{Phase: kwerftv1.UpgradeRunning}}
	if c := checkMap(k3sChecks(t, &fakeAPIServer{}, append(sucObjects(true), busy)...).Kubernetes(ctx, spec, ""))[CheckNoOtherOperation]; c.OK {
		t.Errorf("another upgrade: %+v", c)
	}
}
