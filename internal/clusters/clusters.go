// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package clusters is the seam between the multi-cluster parts of Kwerft:
// the agent tunnel (which knows how to reach a remote cluster's Kubernetes
// API) and the console (which acts as the signed-in user in whichever
// cluster a project lives in). The console depends only on Registry.
package clusters

import (
	"context"
	"errors"
	"sync"

	"k8s.io/client-go/rest"
)

// Local is the management cluster: the one the console runs in.
const Local = "local"

// ErrUnavailable: the cluster is known but cannot be reached now (its
// agent is disconnected).
var ErrUnavailable = errors.New("cluster unavailable")

// ErrUnknown: no cluster of that name.
var ErrUnknown = errors.New("unknown cluster")

// Info is what the console needs to know about a cluster to route to it.
type Info struct {
	Name string
	// Connected: requests can reach its API now.
	Connected bool
}

// Registry knows the managed clusters and how to reach each one's API.
type Registry interface {
	// List returns every managed cluster, Local first.
	List(ctx context.Context) ([]Info, error)
	// RESTConfig reaches the cluster's API server with Kwerft's own
	// identity there: the console's service account for Local, the agent's
	// for remote clusters (whose requests go through its tunnel). Callers
	// add impersonation for user requests. ErrUnavailable while the agent
	// is away, ErrUnknown for names it does not know.
	RESTConfig(name string) (*rest.Config, error)
	// Changed is closed and replaced whenever a cluster connects or
	// disconnects, so per-cluster clients and caches can be (re)built.
	Changed() <-chan struct{}
}

// Static is a Registry with the local cluster only: single-cluster
// installs and tests.
type Static struct {
	Config *rest.Config

	once    sync.Once
	changed chan struct{}
}

func (s *Static) List(context.Context) ([]Info, error) {
	return []Info{{Name: Local, Connected: true}}, nil
}

func (s *Static) RESTConfig(name string) (*rest.Config, error) {
	if name != Local {
		return nil, ErrUnknown
	}
	return rest.CopyConfig(s.Config), nil
}

func (s *Static) Changed() <-chan struct{} {
	s.once.Do(func() { s.changed = make(chan struct{}) })
	return s.changed
}
