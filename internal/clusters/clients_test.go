// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"context"
	"errors"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
)

// swappable is a Registry whose one remote endpoint changes, like a cluster
// whose agent reconnected.
type swappable struct{ cfg *rest.Config }

func (s *swappable) List(context.Context) ([]Info, error) { return nil, nil }
func (s *swappable) Changed() <-chan struct{}             { return nil }
func (s *swappable) RESTConfig(name string) (*rest.Config, error) {
	if s.cfg == nil {
		return nil, ErrUnavailable
	}
	return rest.CopyConfig(s.cfg), nil
}

func TestClientsFollowTheSession(t *testing.T) {
	reg := &swappable{cfg: &rest.Config{Host: "http://127.0.0.1:1", BearerToken: "a"}}
	cs := &Clients{Registry: reg, Scheme: runtime.NewScheme()}
	first, err := cs.For("edge")
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := cs.For("edge"); again != first {
		t.Error("same session, new client")
	}
	reg.cfg = &rest.Config{Host: "http://127.0.0.1:2", BearerToken: "b"}
	if next, _ := cs.For("edge"); next == first {
		t.Error("new session, old client")
	}
	reg.cfg = nil
	if _, err := cs.For("edge"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("disconnected: %v", err)
	}
}
