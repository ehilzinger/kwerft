package controllers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/auth"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// fakeTunnel stands in for the console's clusters.Hub: tests connect and
// disconnect agents by hand.
type fakeTunnel struct {
	mu           sync.Mutex
	agents       map[string]clusters.AgentStatus
	hashes       map[string]string // the token hash each agent connected with
	changed      chan struct{}
	disconnected []string
}

var testTunnel = &fakeTunnel{agents: map[string]clusters.AgentStatus{}, hashes: map[string]string{}, changed: make(chan struct{})}

func (f *fakeTunnel) connect(name, hash string, st clusters.AgentStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[name], f.hashes[name] = st, hash
	close(f.changed)
	f.changed = make(chan struct{})
}

func (f *fakeTunnel) Agent(name string) (clusters.AgentStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.agents[name]
	return st, ok
}

func (f *fakeTunnel) Disconnect(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.agents[name]; ok {
		delete(f.agents, name)
		f.disconnected = append(f.disconnected, name)
		close(f.changed)
		f.changed = make(chan struct{})
	}
}

func (f *fakeTunnel) DisconnectUnless(name, hash string) {
	f.mu.Lock()
	h, ok := f.hashes[name]
	f.mu.Unlock()
	if ok && h != hash {
		f.Disconnect(name)
	}
}

func (f *fakeTunnel) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changed
}

func (f *fakeTunnel) wasDisconnected(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range f.disconnected {
		if n == name {
			return true
		}
	}
	return false
}

func waitForCluster(t *testing.T, name string, check func(*kwerftv1.Cluster) error) *kwerftv1.Cluster {
	t.Helper()
	var c kwerftv1.Cluster
	eventually(t, func() error {
		c = kwerftv1.Cluster{}
		if err := k8s.Get(context.Background(), client.ObjectKey{Name: name}, &c); err != nil {
			return err
		}
		return check(&c)
	})
	return &c
}

func phaseIs(p kwerftv1.ClusterPhase) func(*kwerftv1.Cluster) error {
	return func(c *kwerftv1.Cluster) error {
		if c.Status.Phase != p {
			return fmt.Errorf("phase %q, want %q", c.Status.Phase, p)
		}
		return nil
	}
}

func TestClusterLocalExists(t *testing.T) {
	requireEnvtest(t)
	c := waitForCluster(t, clusters.Local, phaseIs(ClusterConnected))
	if c.Spec.Provider != kwerftv1.ClusterLocal || c.Status.LastSeen == nil || c.Status.KubernetesVersion != "v-test" || c.Status.Nodes != 1 {
		t.Errorf("local cluster = %+v %+v", c.Spec, c.Status)
	}
	// Deleted by hand, it comes back.
	if err := k8s.Delete(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	waitForCluster(t, clusters.Local, func(n *kwerftv1.Cluster) error {
		if n.UID == c.UID {
			return fmt.Errorf("not re-created yet")
		}
		return nil
	})
}

func TestClusterAdoptedFollowsItsAgent(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	token := clusters.NewAgentToken("adopted-1")
	hash := auth.HashToken(token)
	c := &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "adopted-1", Annotations: map[string]string{clusters.TokenHashAnnotation: hash}},
		Spec:       kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitForCluster(t, c.Name, phaseIs(ClusterPending))

	// The agent connects: versions, nodes and join material arrive.
	seen := time.Now()
	testTunnel.connect(c.Name, hash, clusters.AgentStatus{LastSeen: seen, Remote: "203.0.113.7", Info: clusters.AgentInfo{
		AgentVersion: "0.5.0", KubernetesVersion: "v1.37.1+k3s1", Nodes: 3, ReadyNodes: 2,
		Join: &clusters.JoinMaterial{Server: "https://10.1.0.2:6443", Token: "K10secret::server:x"},
	}})
	got := waitForCluster(t, c.Name, phaseIs(ClusterConnected))
	if got.Status.AgentVersion != "0.5.0" || got.Status.KubernetesVersion != "v1.37.1+k3s1" || got.Status.Nodes != 3 || got.Status.ReadyNodes != 2 ||
		got.Status.LastSeen == nil || !ClusterReady(got) {
		t.Errorf("connected status = %+v", got.Status)
	}
	var join corev1.Secret
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: clusters.JoinSecretName(c.Name)}, &join)
	})
	if string(join.Data["server"]) != "https://10.1.0.2:6443" || string(join.Data["token"]) != "K10secret::server:x" || !metav1.IsControlledBy(&join, got) {
		t.Errorf("join secret = %v %v", join.Data, join.OwnerReferences)
	}
	// Adopted clusters never keep their token.
	var agentSecret corev1.Secret
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: clusters.AgentSecretName(c.Name)}, &agentSecret); !apierrors.IsNotFound(err) {
		t.Errorf("adopted cluster has an agent secret: %v", err)
	}

	// Rotating the token (also by hand) disconnects the agent that used the old one.
	patch := client.MergeFrom(got.DeepCopy())
	got.Annotations[clusters.TokenHashAnnotation] = auth.HashToken(clusters.NewAgentToken(c.Name))
	if err := k8s.Patch(ctx, got, patch); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if !testTunnel.wasDisconnected(c.Name) {
			return fmt.Errorf("agent still connected")
		}
		return nil
	})
	waitForCluster(t, c.Name, phaseIs(ClusterDisconnected))

	// Deleting the cluster removes its secrets and lets it go.
	if err := k8s.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: c.Name}, &kwerftv1.Cluster{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("cluster still there: %v", err)
		}
		return nil
	})
	if err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: clusters.JoinSecretName(c.Name)}, &join); !apierrors.IsNotFound(err) {
		t.Errorf("join secret left behind: %v", err)
	}
}

func TestClusterOnHetznerCloud(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	c := &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cloud-1"},
		Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterHetznerCloud,
			HetznerCloud: &kwerftv1.HetznerClusterSpec{Location: "fsn1", ServerType: "cx32", ControlPlanes: 3}},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	got := waitForCluster(t, c.Name, phaseIs(ClusterProvisioning))
	if !strings.Contains(got.Status.Conditions[0].Message, "control plane") {
		t.Errorf("provisioning message = %q", got.Status.Conditions[0].Message)
	}

	// The control-plane pool, owned by the cluster.
	var pool kwerftv1.NodePool
	eventually(t, func() error { return k8s.Get(ctx, client.ObjectKey{Name: ControlPlanePoolName(c.Name)}, &pool) })
	if pool.Spec.Cluster != c.Name || pool.Spec.Role != kwerftv1.NodeControlPlane || pool.Spec.Count != 3 ||
		pool.Spec.ServerType != "cx32" || pool.Spec.Location != "fsn1" || !metav1.IsControlledBy(&pool, got) {
		t.Errorf("control-plane pool = %+v", pool.Spec)
	}

	// The token for the first server's cloud-init, and its hash on the cluster.
	var secret corev1.Secret
	eventually(t, func() error {
		return k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: clusters.AgentSecretName(c.Name)}, &secret)
	})
	token := string(secret.Data[clusters.SecretToken])
	if name, ok := clusters.AgentTokenCluster(token); !ok || name != c.Name {
		t.Errorf("agent token %q", token)
	}
	if !strings.HasPrefix(string(secret.Data[clusters.SecretConsoleURL]), "https://") {
		t.Errorf("console URL %q", secret.Data[clusters.SecretConsoleURL])
	}
	got = waitForCluster(t, c.Name, func(c *kwerftv1.Cluster) error {
		if !auth.TokenMatches(token, c.Annotations[clusters.TokenHashAnnotation]) {
			return fmt.Errorf("token hash %q does not match the secret", c.Annotations[clusters.TokenHashAnnotation])
		}
		return nil
	})

	// More control planes (1 → 3 or back) change the pool.
	patch := client.MergeFrom(got.DeepCopy())
	got.Spec.HetznerCloud.ControlPlanes = 1
	if err := k8s.Patch(ctx, got, patch); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: ControlPlanePoolName(c.Name)}, &pool); err != nil {
			return err
		}
		if pool.Spec.Count != 1 {
			return fmt.Errorf("count %d", pool.Spec.Count)
		}
		return nil
	})

	// Once the agent connected, the plain token is gone.
	testTunnel.connect(c.Name, got.Annotations[clusters.TokenHashAnnotation], clusters.AgentStatus{LastSeen: time.Now(), Info: clusters.AgentInfo{Nodes: 1, ReadyNodes: 1}})
	waitForCluster(t, c.Name, phaseIs(ClusterConnected))
	eventually(t, func() error {
		err := k8s.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: clusters.AgentSecretName(c.Name)}, &corev1.Secret{})
		if !apierrors.IsNotFound(err) {
			return fmt.Errorf("agent secret still there: %v", err)
		}
		return nil
	})

	// A worker pool someone added for this cluster.
	worker := &kwerftv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "cloud-1-workers"},
		Spec: kwerftv1.NodePoolSpec{Cluster: c.Name, Role: kwerftv1.NodeWorker, ServerType: "cx22", Location: "fsn1", Count: 2}}
	if err := k8s.Create(ctx, worker); err != nil {
		t.Fatal(err)
	}

	// Deleting the cluster disconnects it and deletes every pool of it.
	if err := k8s.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() error {
		if err := k8s.Get(ctx, client.ObjectKey{Name: c.Name}, &kwerftv1.Cluster{}); !apierrors.IsNotFound(err) {
			return fmt.Errorf("cluster still there: %v", err)
		}
		return nil
	})
	if !testTunnel.wasDisconnected(c.Name) {
		t.Error("agent not disconnected on delete")
	}
	for _, name := range []string{ControlPlanePoolName(c.Name), worker.Name} {
		var p kwerftv1.NodePool
		if err := k8s.Get(ctx, client.ObjectKey{Name: name}, &p); !apierrors.IsNotFound(err) && p.DeletionTimestamp == nil {
			t.Errorf("pool %s not deleted: %v", name, err)
		}
	}
}

func TestClusterNamesAreChecked(t *testing.T) {
	requireEnvtest(t)
	c := &kwerftv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "not.a.label"}, Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted}}
	if err := k8s.Create(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	got := waitForCluster(t, c.Name, phaseIs(ClusterFailed))
	if reason, _ := readyReason(got.Status.Conditions, got.Generation); reason != "InvalidName" {
		t.Errorf("reason %q", reason)
	}
}
