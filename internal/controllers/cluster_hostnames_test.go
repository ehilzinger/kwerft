// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/clusters"
)

// TestClusterReportsRemoteHostnames: a connected remote cluster gets a
// ConsoleSettings (so its Domain reconciler reports where it is reached), and
// its addresses and the hostnames under the console's apps domain its
// Domains hold come back into the Cluster's status; they stay while the
// agent is away.
func TestClusterReportsRemoteHostnames(t *testing.T) {
	requireEnvtest(t)
	ctx := context.Background()
	useSettings(t, kwerftv1.ConsoleSettingsSpec{AppsDomain: "apps.example.com"})
	remote := fake.NewClientBuilder().WithScheme(NewScheme()).
		WithStatusSubresource(&kwerftv1.ConsoleSettings{}, &kwerftv1.Domain{}).Build()
	domain := func(ns, host string, won bool) {
		t.Helper()
		d := &kwerftv1.Domain{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "web"}, Spec: kwerftv1.DomainSpec{Hostname: host}}
		if err := remote.Create(ctx, d); err != nil {
			t.Fatal(err)
		}
		if won {
			d.Status.Listener, d.Status.ObservedGeneration = ListenerName(host), d.Generation
			if err := remote.Status().Update(ctx, d); err != nil {
				t.Fatal(err)
			}
		}
	}
	domain("maps", "router.apps.example.com", true)
	domain("tiles", "tiles.apps.example.com", false) // no listener: nothing to point at yet
	domain("site", "www.example.org", true)          // not under the apps domain

	c := &kwerftv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: "ingress-1", Annotations: map[string]string{clusters.TokenHashAnnotation: "x"}},
		Spec:       kwerftv1.ClusterSpec{Provider: kwerftv1.ClusterAdopted},
	}
	if err := k8s.Create(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k8s.Delete(context.Background(), c) })
	testTunnel.setRemote(c.Name, remote)
	testTunnel.connect(c.Name, "x", clusters.AgentStatus{LastSeen: time.Now()})

	// The cluster's ConsoleSettings appears; its Domain reconciler reports.
	eventually(t, func() error {
		var s kwerftv1.ConsoleSettings
		if err := remote.Get(ctx, client.ObjectKey{Name: kwerftv1.ConsoleSettingsName}, &s); err != nil {
			return err
		}
		s.Status.PublicAddresses = []string{"198.51.100.7"}
		return remote.Status().Update(ctx, &s)
	})
	waitForCluster(t, c.Name, func(c *kwerftv1.Cluster) error {
		if !slices.Equal(c.Status.PublicAddresses, []string{"198.51.100.7"}) {
			return fmt.Errorf("addresses %v", c.Status.PublicAddresses)
		}
		if len(c.Status.Hostnames) != 1 || c.Status.Hostnames[0].Hostname != "router.apps.example.com" || c.Status.Hostnames[0].Project != "maps" {
			return fmt.Errorf("hostnames %+v", c.Status.Hostnames)
		}
		return nil
	})

	// The agent goes away: what it last reported stays, so its records do.
	testTunnel.Disconnect(c.Name)
	got := waitForCluster(t, c.Name, phaseIs(ClusterDisconnected))
	if len(got.Status.Hostnames) != 1 || len(got.Status.PublicAddresses) != 1 {
		t.Errorf("status after the agent left: %+v %v", got.Status.Hostnames, got.Status.PublicAddresses)
	}
}
