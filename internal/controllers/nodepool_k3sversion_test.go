package controllers

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/jointoken"
)

func TestK3sVersion(t *testing.T) {
	for in, want := range map[string]string{
		"v1.37.1+k3s1":     "v1.37.1+k3s1",
		"v1.38.0-rc2+k3s1": "v1.38.0-rc2+k3s1",
		"":                 "",
		"v1.37.1":          "", // not k3s (an adopted cluster of another distribution)
		"v1.37.1+rke2r1":   "",
		"v1.37.1+k3s1 ; x": "",
	} {
		if got := k3sVersion(in); got != want {
			t.Errorf("k3sVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestJoinScriptK3sVersion(t *testing.T) {
	j := joinScript{Console: "https://ops.example.com", Mode: "join", Token: "kwft_join_a.b", Role: "worker"}
	if slices.Contains(j.args(), "--k3s-version") {
		t.Fatalf("args without a version: %v", j.args())
	}
	j.K3sVersion = "v1.37.1+k3s1"
	if !strings.Contains(strings.Join(j.args(), " "), "--platform cloud --k3s-version v1.37.1+k3s1") {
		t.Fatalf("join args: %v", j.args())
	}
	j = joinScript{Console: "https://ops.example.com", Mode: "agent", Token: "kwag_x_y", Version: "0.6.0", K3sVersion: "v1.37.1+k3s1"}
	if !strings.Contains(strings.Join(j.args(), " "), "--version 0.6.0 --k3s-version v1.37.1+k3s1") {
		t.Fatalf("agent args: %v", j.args())
	}
}

// A node pool's new servers install the k3s version the cluster reports,
// and the installer's pin while it reports none.
func TestNodePoolUserDataK3sVersion(t *testing.T) {
	ctx := context.Background()
	cl := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "edge"}}
	cl.Status.KubernetesVersion = "v1.37.2+k3s1"
	pool := &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "edge-workers"},
		Spec: kwerftv1.NodePoolSpec{Cluster: "edge", Role: kwerftv1.NodeWorker}}
	c := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(cl).WithStatusSubresource(cl).Build()
	r := &NodePoolReconciler{Client: c, APIReader: c, Namespace: GatewayNamespace,
		JoinTokens: jointoken.NewSigner([]byte("test data key, 32 bytes long....")),
		ConsoleURL: func() string { return testConsoleURL }}
	ud, err := r.userData(ctx, &poolRun{r: r, pool: pool}, "join", "edge-edge-workers-n0001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, "'--k3s-version' 'v1.37.2+k3s1'") {
		t.Fatalf("user data lacks the cluster's k3s version:\n%s", ud)
	}

	// No Cluster object (or no version yet): no flag.
	pool.Spec.Cluster = "other"
	ud, err = r.userData(ctx, &poolRun{r: r, pool: pool}, "join", "other-edge-workers-n0002")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ud, "--k3s-version") {
		t.Fatalf("user data has a k3s version:\n%s", ud)
	}

	// The bootstrap server of a new cluster: the agent secret, no version yet.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: AgentSecretName("other"), Namespace: GatewayNamespace},
		Data: map[string][]byte{"token": []byte("kwag_other_abc")}}
	if err := c.Create(ctx, sec); err != nil {
		t.Fatal(err)
	}
	ud, err = r.userData(ctx, &poolRun{r: r, pool: pool}, "agent", "other-edge-workers-n0003")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ud, "'--agent'") || strings.Contains(ud, "--k3s-version") {
		t.Fatalf("bootstrap user data:\n%s", ud)
	}
}
