package controllers

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// TestClusterHandsHetznerCloudToRemote: a connected hetzner-cloud cluster (a
// second API server) gets the console's Cloud API token as its
// kube-system/hcloud and its Cloud settings in its ConsoleSettings, and its
// report comes back into the Cluster's status.
func TestClusterHandsHetznerCloudToRemote(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "charts", "kwerft", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	remote, err := client.New(cfg, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	// The CSI controller there, which a new token restarts.
	one := int32(1)
	labels := map[string]string{"app": "hcloud-csi-controller"}
	csi := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: HCloudSystemNamespace, Name: "hcloud-csi-controller"},
		Spec: appsv1.DeploymentSpec{Replicas: &one, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "csi", Image: "hcloud-csi"}}}}}}
	if err := remote.Create(ctx, csi); err != nil {
		t.Fatal(err)
	}

	setToken := func(token string) {
		t.Helper()
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: HCloudTokenSecret}}
		eventually(t, func() error {
			var cur corev1.Secret
			if err := k8s.Get(ctx, client.ObjectKeyFromObject(sec), &cur); err != nil {
				sec.Data = map[string][]byte{HCloudTokenKey: []byte(token)}
				return k8s.Create(ctx, sec)
			}
			cur.Data = map[string][]byte{HCloudTokenKey: []byte(token)}
			return k8s.Update(ctx, &cur)
		})
	}
	setToken("token-one")
	t.Cleanup(func() {
		_ = k8s.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: HCloudTokenSecret}})
	})

	c := &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "hcloud-1", Annotations: map[string]string{clusters.TokenHashAnnotation: "x"}},
		Spec: kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterHetznerCloud, HetznerCloud: &kwerftv1.HetznerClusterSpec{
			Location: "fsn1", ServerType: "cx23", ControlPlanes: 1,
			Firewall: kwerftv1.CloudFirewallOff, LoadBalancer: &kwerftv1.LoadBalancerSettings{Enabled: true, Type: "lb11"}}},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), c) })
	testTunnel.setRemote(c.Name, remote)
	testTunnel.connect(c.Name, "x", clusters.AgentStatus{LastSeen: time.Now()})

	remoteToken := func(want string) func() error {
		return func() error {
			var s corev1.Secret
			if err := remote.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &s); err != nil {
				return err
			}
			if got := string(s.Data[HCloudTokenKey]); got != want {
				return fmt.Errorf("remote token %q, want %q", got, want)
			}
			if s.Labels[LabelManagedBy] != ManagedByKwerft {
				return fmt.Errorf("labels %v", s.Labels)
			}
			return nil
		}
	}
	remoteSettings := func(check func(*kwerftv1.ConsoleSettings) error) {
		t.Helper()
		eventually(t, func() error {
			var s kwerftv1.ConsoleSettings
			if err := remote.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
				return err
			}
			return check(&s)
		})
	}
	condition := func(status metav1.ConditionStatus, reason string) func(*kwerftv1.Cluster) error {
		return func(c *kwerftv1.Cluster) error {
			cond := meta.FindStatusCondition(c.Status.Conditions, ConditionHetznerCloud)
			if cond == nil || cond.Status != status || cond.Reason != reason {
				return fmt.Errorf("condition %+v", cond)
			}
			return nil
		}
	}

	// The token, as the CCM's and CSI driver's Secret; the settings.
	eventually(t, remoteToken("token-one"))
	remoteSettings(func(s *kwerftv1.ConsoleSettings) error {
		h := s.Spec.HetznerCloud
		if h == nil || h.Firewall != kwerftv1.CloudFirewallOff || h.LoadBalancer == nil || !h.LoadBalancer.Enabled || h.LoadBalancer.Type != "lb11" {
			return fmt.Errorf("spec.hetznerCloud %+v", h)
		}
		if s.Annotations[AnnotationHCloudTokenSum] != tokenSum("token-one") {
			return fmt.Errorf("annotations %v", s.Annotations)
		}
		return nil
	})
	waitForCluster(t, c.Name, condition(metav1.ConditionTrue, "Synced"))

	// The cluster's report comes back.
	eventually(t, func() error {
		var s kwerftv1.ConsoleSettings
		if err := remote.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		s.Status.HetznerCloud = &kwerftv1.HetznerCloudStatus{Firewall: &kwerftv1.CloudFirewallStatus{State: "Off"}, Message: "from the cluster"}
		return remote.Status().Update(ctx, &s)
	})
	waitForCluster(t, c.Name, func(c *kwerftv1.Cluster) error {
		if h := c.Status.HetznerCloud; h == nil || h.Message != "from the cluster" || h.Firewall == nil || h.Firewall.State != "Off" {
			return fmt.Errorf("status.hetznerCloud %+v", h)
		}
		return nil
	})

	// Settings changed in the console reach the cluster.
	eventually(t, func() error {
		var cur kwerftv1.Cluster
		if err := k8s.Get(ctx, client.ObjectKeyFromObject(c), &cur); err != nil {
			return err
		}
		cur.Spec.HetznerCloud.Firewall, cur.Spec.HetznerCloud.LoadBalancer = kwerftv1.CloudFirewallSync, nil
		return k8s.Update(ctx, &cur)
	})
	remoteSettings(func(s *kwerftv1.ConsoleSettings) error {
		if h := s.Spec.HetznerCloud; h == nil || h.Firewall != kwerftv1.CloudFirewallSync || h.LoadBalancer != nil {
			return fmt.Errorf("spec.hetznerCloud %+v", h)
		}
		return nil
	})

	// A new token: the Secret, the change marker, and a restart of the CSI
	// controller there.
	setToken("token-two")
	eventually(t, remoteToken("token-two"))
	remoteSettings(func(s *kwerftv1.ConsoleSettings) error {
		if s.Annotations[AnnotationHCloudTokenSum] != tokenSum("token-two") {
			return fmt.Errorf("annotations %v", s.Annotations)
		}
		return nil
	})
	eventually(t, func() error {
		var d appsv1.Deployment
		if err := remote.Get(ctx, client.ObjectKeyFromObject(csi), &d); err != nil {
			return err
		}
		if got := d.Spec.Template.Annotations[annotationHCloudTokenSum]; got != tokenSum("token-two") {
			return fmt.Errorf("restart annotation %q", got)
		}
		return nil
	})

	// The cluster's own kube-system/hcloud is left alone.
	eventually(t, func() error {
		var s corev1.Secret
		if err := remote.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &s); err != nil {
			return err
		}
		s.Labels = map[string]string{"owner": "operator"}
		return remote.Update(ctx, &s)
	})
	setToken("token-three")
	waitForCluster(t, c.Name, condition(metav1.ConditionFalse, "Incomplete"))
	var s corev1.Secret
	if err := remote.Get(ctx, client.ObjectKey{Namespace: HCloudSystemNamespace, Name: HCloudSystemSecret}, &s); err != nil || string(s.Data[HCloudTokenKey]) != "token-two" {
		t.Fatalf("the cluster's own Secret was changed: %q %v", s.Data[HCloudTokenKey], err)
	}

	// No token stored in the console: reported.
	if err := k8s.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: GatewayNamespace, Name: HCloudTokenSecret}}); err != nil {
		t.Fatal(err)
	}
	waitForCluster(t, c.Name, func(c *kwerftv1.Cluster) error {
		cond := meta.FindStatusCondition(c.Status.Conditions, ConditionHetznerCloud)
		if cond == nil || cond.Reason != "Incomplete" || !strings.HasPrefix(cond.Message, "No Hetzner Cloud API token") {
			return fmt.Errorf("condition %+v", cond)
		}
		return nil
	})
}
