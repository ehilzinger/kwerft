// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"context"
	"errors"
	"testing"

	"k8s.io/client-go/rest"
)

func TestStatic(t *testing.T) {
	s := &Static{Config: &rest.Config{Host: "https://local"}}
	list, _ := s.List(context.Background())
	if len(list) != 1 || list[0].Name != Local || !list[0].Connected {
		t.Errorf("list = %+v", list)
	}
	if c, err := s.RESTConfig(Local); err != nil || c.Host != "https://local" {
		t.Errorf("local: %v %v", c, err)
	}
	if _, err := s.RESTConfig("other"); !errors.Is(err, ErrUnknown) {
		t.Errorf("other: %v", err)
	}
	if s.Changed() == nil {
		t.Error("no change channel")
	}
}
