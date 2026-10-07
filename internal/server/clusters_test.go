// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"k8s.io/client-go/rest"

	"github.com/ehilzinger/kwerft/internal/clusters"
)

// TestClusterSetFollowsTheRegistry: connections are built when a cluster
// connects, kept while it stays, stopped when it goes, and retried after a
// failed connect.
func TestClusterSetFollowsTheRegistry(t *testing.T) {
	reg := newTestRegistry()
	now := time.Unix(1700000000, 0)
	s := newClusterSet(reg, &clusterConn{name: clusters.Local}, slog.New(slog.DiscardHandler), func() time.Time { return now })
	var built, stopped []string
	fail := true
	s.connect = func(name string, cfg *rest.Config) (*clusterConn, error) {
		if fail {
			return nil, errors.New("no route")
		}
		built = append(built, name)
		return &clusterConn{name: name, host: cfg.Host, stop: func() { stopped = append(stopped, name) }}, nil
	}

	reg.add("a", &rest.Config{Host: "https://a"})
	if _, err := s.byName("a"); !errors.Is(err, clusters.ErrUnavailable) {
		t.Fatalf("a, connect failing: %v", err)
	}
	fail = false
	if _, err := s.byName("a"); !errors.Is(err, clusters.ErrUnavailable) {
		t.Fatalf("a, before the retry is due: %v", err)
	}
	now = now.Add(retryAfter + time.Second)
	if c, err := s.byName("a"); err != nil || c.name != "a" {
		t.Fatalf("a after the retry: %v %v", c, err)
	}
	reg.add("b", &rest.Config{Host: "https://b"})
	if _, err := s.byName("b"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(built, []string{"a", "b"}) || len(stopped) != 0 {
		t.Errorf("built %v, stopped %v: a must be kept while b connects", built, stopped)
	}
	if !s.multi() {
		t.Error("not multi with three clusters")
	}
	reg.setConnected("a", false)
	if _, err := s.byName("a"); !errors.Is(err, clusters.ErrUnavailable) {
		t.Errorf("a after it went: %v", err)
	}
	if !slices.Equal(stopped, []string{"a"}) {
		t.Errorf("stopped %v, want a", stopped)
	}
	if _, err := s.byName("nowhere"); !errors.Is(err, clusters.ErrUnknown) {
		t.Errorf("an unknown cluster: %v", err)
	}
	if c, err := s.byName(""); err != nil || !c.isLocal() {
		t.Errorf("no name: %v %v", c, err)
	}
	var names []string
	for _, st := range s.states() {
		names = append(names, st.name)
	}
	if !slices.Equal(names, []string{"local", "a", "b"}) {
		t.Errorf("states %v", names)
	}
}

// TestNoRegistryIsSingleCluster: without a Registry nothing is looked up.
func TestNoRegistryIsSingleCluster(t *testing.T) {
	local := &clusterConn{name: clusters.Local}
	s := newClusterSet(nil, local, slog.New(slog.DiscardHandler), time.Now)
	if s.multi() {
		t.Error("multi without a registry")
	}
	if c, err := s.forProject(context.Background(), "anything"); err != nil || c != local {
		t.Errorf("forProject: %v %v", c, err)
	}
}

func TestClusterErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{&clusterUnreachableError{cluster: "edge", project: "shop"}, http.StatusServiceUnavailable},
		{&clusterUnknownError{cluster: "edge"}, http.StatusNotFound},
		{&projectConflictError{project: "shop", clusters: []string{"edge", "local"}}, http.StatusConflict},
	} {
		w := httptest.NewRecorder()
		clusterError(w, tc.err)
		if w.Code != tc.code {
			t.Errorf("%T: %d, want %d", tc.err, w.Code, tc.code)
		}
	}
	if !errors.Is(&clusterUnreachableError{cluster: "x"}, clusters.ErrUnavailable) {
		t.Error("unreachable is not ErrUnavailable")
	}
}
