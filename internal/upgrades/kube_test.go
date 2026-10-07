// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kwerftv1.AddToScheme(s)
	_ = apiextensionsv1.AddToScheme(s)
	return s
}

func readyDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 2},
		Spec: appsv1.DeploymentSpec{Replicas: ptr.To[int32](1), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "kwerft", Image: "ghcr.io/ehilzinger/kwerft:0.6.0"}}}}},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1, ReadyReplicas: 1},
	}
}

type verifyEnv struct {
	c       client.Client
	k       *KubeCluster
	version string
}

func newVerifyEnv(t *testing.T, objs ...client.Object) *verifyEnv {
	t.Helper()
	e := &verifyEnv{version: "0.6.0"}
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/version" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"version":"` + e.version + `","commit":"abc"}`))
	}))
	t.Cleanup(console.Close)
	public := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusFound) }))
	t.Cleanup(public.Close)

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "upgrades.kwerft.dev"},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Group: "kwerft.dev"},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
			{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue}}},
	}
	other := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "gateways.gateway.networking.k8s.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: "gateway.networking.k8s.io"}}
	agent := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kwerft-system", Name: nodeAgentDS, Generation: 3},
		Status: appsv1.DaemonSetStatus{ObservedGeneration: 3, DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberAvailable: 2}}
	app := &kwerftv1.App{ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"}, Status: kwerftv1.AppStatus{ReadyReplicas: 2}}
	base := []client.Object{readyDeployment("kwerft-system", consoleDeployment), readyDeployment(hubbleNamespace, hubbleRelay),
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kwerft-system", Name: consoleService}, Spec: corev1.ServiceSpec{ClusterIP: "10.43.0.10"}},
		crd, other, agent, app}
	e.c = fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(append(base, objs...)...).
		WithStatusSubresource(&kwerftv1.Upgrade{}).Build()
	e.k = &KubeCluster{Client: e.c, Name: "u", Namespace: "kwerft-system",
		HTTP: public.Client(), ConsoleServiceURL: console.URL, PublicURL: public.URL + "/"}
	return e
}

func TestVerifyPasses(t *testing.T) {
	e := newVerifyEnv(t)
	if p := e.k.Verify(context.Background(), VerifyTarget{Version: "0.6.0", Apps: map[string]int32{"shop/web": 2, "gone/app": 1}}); len(p) != 0 {
		t.Fatalf("problems: %v", p)
	}
}

func TestVerifyFindsProblems(t *testing.T) {
	ctx := context.Background()
	e := newVerifyEnv(t)
	e.version = "0.5.0"
	var app kwerftv1.App
	_ = e.c.Get(ctx, client.ObjectKey{Namespace: "shop", Name: "web"}, &app)
	app.Status.ReadyReplicas = 1
	if err := e.c.Update(ctx, &app); err != nil {
		t.Fatal(err)
	}
	var ds appsv1.DaemonSet
	_ = e.c.Get(ctx, client.ObjectKey{Namespace: "kwerft-system", Name: nodeAgentDS}, &ds)
	ds.Status.UpdatedNumberScheduled = 1
	if err := e.c.Status().Update(ctx, &ds); err != nil {
		t.Fatal(err)
	}
	broken := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: "secretsets.kwerft.dev"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Group: "kwerft.dev"}}
	if err := e.c.Create(ctx, broken); err != nil {
		t.Fatal(err)
	}
	e.k.PublicURL = "https://127.0.0.1:1/"

	p := strings.Join(e.k.Verify(ctx, VerifyTarget{Version: "0.6.0", Apps: map[string]int32{"shop/web": 2}}), "\n")
	for _, want := range []string{"reports 0.5.0, not 0.6.0", "node agent is rolling out (1 of 2", "secretsets.kwerft.dev",
		"does not answer with a valid certificate", "shop/web (1 of 2 ready)"} {
		if !strings.Contains(p, want) {
			t.Errorf("problems lack %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "gateways") {
		t.Error("checked CRDs that are not Kwerft's")
	}
}

func TestVerifyAgentReadsTheImageTag(t *testing.T) {
	ctx := context.Background()
	e := newVerifyEnv(t)
	_ = e.c.Delete(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "kwerft-system", Name: consoleService}})
	e.version = "wrong" // the Service is not asked
	if p := e.k.Verify(ctx, VerifyTarget{Version: "0.6.0"}); len(p) != 0 {
		t.Errorf("agent: %v", p)
	}
	if p := e.k.Verify(ctx, VerifyTarget{Version: "0.5.0"}); len(p) != 1 {
		t.Errorf("agent on the wrong version: %v", p)
	}
}

func TestKubeClusterStatusAndLog(t *testing.T) {
	ctx := context.Background()
	u := &kwerftv1.Upgrade{ObjectMeta: metav1.ObjectMeta{Name: "u", UID: "uid-1"},
		Spec: kwerftv1.UpgradeSpec{Component: kwerftv1.UpgradeKwerft, Version: "0.6.0"}}
	e := newVerifyEnv(t, u)
	if _, err := e.k.UpdateStatus(ctx, func(u *kwerftv1.Upgrade) error {
		u.Status.Phase = kwerftv1.UpgradeRunning
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.k.Annotate(ctx, AnnotationRestoreDatabase, "0.5.0"); err != nil {
		t.Fatal(err)
	}
	got, err := e.k.Upgrade(ctx)
	if err != nil || got.Status.Phase != kwerftv1.UpgradeRunning || got.Annotations[AnnotationRestoreDatabase] != "0.5.0" {
		t.Fatalf("upgrade = %+v %v", got, err)
	}
	for _, text := range []string{"first", "second \xff"} {
		if err := e.k.WriteLog(ctx, []byte(text)); err != nil {
			t.Fatal(err)
		}
	}
	var cm corev1.ConfigMap
	if err := e.c.Get(ctx, client.ObjectKey{Namespace: "kwerft-system", Name: "u-log"}, &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data[LogKey] != "second ?" || cm.OwnerReferences[0].UID != "uid-1" || cm.Labels[LabelUpgrade] != "u" {
		t.Errorf("log ConfigMap = %+v", cm)
	}
	apps, _ := e.k.AppReplicas(ctx)
	if apps["shop/web"] != 2 {
		t.Errorf("apps = %v", apps)
	}
}
