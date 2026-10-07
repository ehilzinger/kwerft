// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hetzner_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ehilzinger/kwerft/internal/hetzner"
	"github.com/ehilzinger/kwerft/internal/hetzner/hetznertest"
)

func TestMatchZone(t *testing.T) {
	zones := []hetzner.Zone{{Name: "example.com"}, {Name: "apps.example.com."}, {Name: "example.org"}}
	for _, tc := range []struct {
		host, zone, name string
		ok               bool
	}{
		{"ops.example.com", "example.com", "ops", true},
		{"example.com", "example.com", "@", true},
		{"*.apps.example.com", "apps.example.com", "*", true}, // the longest suffix wins
		{"a.b.example.org", "example.org", "a.b", true},
		{"badexample.com", "", "", false},
		{"example.net", "", "", false},
	} {
		z, name, ok := hetzner.MatchZone(zones, tc.host)
		if ok != tc.ok || z.Name != tc.zone || name != tc.name {
			t.Errorf("%s: got %q %q %v", tc.host, z.Name, name, ok)
		}
	}
}

func TestClientAgainstFake(t *testing.T) {
	srv := hetznertest.New(t, "tok", "example.com")
	c := srv.Client()
	ctx := context.Background()

	if zone, err := c.ZoneFor(ctx, "a.b.example.com"); err != nil || zone != "example.com" {
		t.Errorf("ZoneFor = %q, %v", zone, err)
	}
	if _, err := c.ZoneFor(ctx, "example.net"); !errors.Is(err, hetzner.ErrNoZone) {
		t.Errorf("ZoneFor(example.net) = %v", err)
	}
	ttl := 300
	if err := c.CreateRRSet(ctx, "example.com", hetzner.RRSet{Name: "ops", Type: "A", TTL: &ttl,
		Records: []hetzner.Record{{Value: "203.0.113.24"}}}); err != nil {
		t.Fatal(err)
	}
	var apiErr *hetzner.APIError
	if err := c.CreateRRSet(ctx, "example.com", hetzner.RRSet{Name: "ops", Type: "A",
		Records: []hetzner.Record{{Value: "203.0.113.24"}}}); !errors.As(err, &apiErr) || apiErr.Code != "uniqueness_error" {
		t.Errorf("second create = %v", err)
	}
	if err := c.SetRecords(ctx, "example.com", "ops", "A", []hetzner.Record{{Value: "198.51.100.7"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetLabels(ctx, "example.com", "ops", "A", map[string]string{"kwerft.dev/managed-by": "kwerft"}); err != nil {
		t.Fatal(err)
	}
	sets, err := c.RRSets(ctx, "example.com", "A", "AAAA")
	if err != nil || len(sets) != 1 || sets[0].Values()[0] != "198.51.100.7" || sets[0].Labels["kwerft.dev/managed-by"] != "kwerft" {
		t.Errorf("RRSets = %+v, %v", sets, err)
	}
	if err := c.DeleteRRSet(ctx, "example.com", "ops", "A"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteRRSet(ctx, "example.com", "ops", "A"); err != nil {
		t.Errorf("deleting a missing RRset: %v", err)
	}

	srv.SetToken("other")
	if _, err := c.Zones(ctx); !errors.Is(err, hetzner.ErrTokenRejected) {
		t.Errorf("wrong token: %v", err)
	}
	srv.FailWith(http.StatusTooManyRequests)
	var rl *hetzner.RateLimitError
	if _, err := c.Zones(ctx); !errors.As(err, &rl) {
		t.Errorf("rate limit: %v", err)
	}
}
