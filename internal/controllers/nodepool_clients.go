// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/clusters"
)

// ClusterClients reaches a cluster's Kubernetes API with Kwerft's own
// identity there: the management cluster's for "local", the agent's for
// remote clusters (through its tunnel). The clients read uncached, so pods
// can be listed by node.
type ClusterClients interface {
	For(ctx context.Context, cluster string) (client.Client, error)
}

// RegistryClients builds ClusterClients from a clusters.Registry and keeps
// one client per cluster until a cluster connects or disconnects.
type RegistryClients struct {
	Registry clusters.Registry
	Scheme   *runtime.Scheme

	mu      sync.Mutex
	clients map[string]client.Client
	changed <-chan struct{}
}

func (r *RegistryClients) For(_ context.Context, cluster string) (client.Client, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.changed:
		r.clients = nil
	default:
	}
	if r.clients == nil {
		r.clients = map[string]client.Client{}
		r.changed = r.Registry.Changed()
	}
	if c, ok := r.clients[cluster]; ok {
		return c, nil
	}
	cfg, err := r.Registry.RESTConfig(cluster)
	if err != nil {
		return nil, err
	}
	scheme := r.Scheme
	if scheme == nil {
		scheme = NewScheme()
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	r.clients[cluster] = c
	return c, nil
}

// StaticClients maps cluster names to clients (tests).
type StaticClients map[string]client.Client

func (s StaticClients) For(_ context.Context, cluster string) (client.Client, error) {
	if c, ok := s[cluster]; ok {
		return c, nil
	}
	return nil, clusters.ErrUnavailable
}
