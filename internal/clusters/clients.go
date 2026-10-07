// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Clients keeps one controller-runtime client per cluster on a Registry,
// with Kwerft's own identity there, and builds a new one when the cluster's
// endpoint changes (its agent connected again).
type Clients struct {
	Registry Registry
	Scheme   *runtime.Scheme

	mu     sync.Mutex
	byName map[string]cachedClient
}

type cachedClient struct {
	host, token string
	c           client.Client
}

// For returns the client for a cluster; the Registry's ErrUnavailable and
// ErrUnknown pass through.
func (cs *Clients) For(name string) (client.Client, error) {
	cfg, err := cs.Registry.RESTConfig(name)
	if err != nil {
		return nil, err
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c, ok := cs.byName[name]; ok && c.host == cfg.Host && c.token == cfg.BearerToken {
		return c.c, nil
	}
	c, err := client.New(cfg, client.Options{Scheme: cs.Scheme})
	if err != nil {
		return nil, err
	}
	if cs.byName == nil {
		cs.byName = map[string]cachedClient{}
	}
	cs.byName[name] = cachedClient{host: cfg.Host, token: cfg.BearerToken, c: c}
	return c, nil
}
