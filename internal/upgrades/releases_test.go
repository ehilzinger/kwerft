// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

func TestVersionOrder(t *testing.T) {
	ordered := []string{"0.4.9", "0.5.0-rc.1", "0.5.0-rc.2", "0.5.0", "0.5.1", "0.10.0", "1.0.0"}
	for i := 1; i < len(ordered); i++ {
		a, b := MustVersion(ordered[i-1]), MustVersion(ordered[i])
		if !a.Less(b) || b.Less(a) {
			t.Errorf("%s should be older than %s", a, b)
		}
	}
	k1, k2 := MustVersion("v1.38.1+k3s1"), MustVersion("v1.38.1+k3s2")
	if !k1.Less(k2) || k1.MinorString() != "1.38" || MustVersion("v1.39.0+k3s1").MinorsAhead(k1) != 1 {
		t.Error("k3s builds order by their number")
	}
	for _, bad := range []string{"", "1.2", "1.2.x", "01.2.3", "1.2.3-"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	for v, want := range map[string]bool{"0.6.0": true, "v0.6.0": true, "0.6.0-rc.1": true, "0.1.0-dev": false,
		"0.5.0-3-gabc1234": false, "0.5.0-dirty": false, "abc1234": false} {
		if IsRelease(v) != want {
			t.Errorf("IsRelease(%q) = %v", v, !want)
		}
	}
}

// fakeSource is the install repository.
type fakeSource struct {
	releases  []Release
	manifests map[string]*Manifest
	notes     map[string]string
	calls     atomic.Int32
	err       error
}

func (f *fakeSource) Releases(context.Context) ([]Release, error) {
	f.calls.Add(1)
	return f.releases, f.err
}

func (f *fakeSource) Manifest(_ context.Context, v string) (*Manifest, error) {
	f.calls.Add(1)
	if m, ok := f.manifests[v]; ok {
		return m, nil
	}
	return nil, fmt.Errorf("manifest %s: %w", v, ErrNotFound)
}

func (f *fakeSource) Notes(_ context.Context, v string) (string, error) {
	f.calls.Add(1)
	if n, ok := f.notes[v]; ok {
		return n, nil
	}
	return "", ErrNotFound
}

func manifest(version, upgradeFrom, pinned string, supported ...string) *Manifest {
	m := &Manifest{Version: version, Channel: ChannelStable, UpgradeFrom: upgradeFrom,
		Image: "ghcr.io/ehilzinger/kwerft@sha256:" + strings.Repeat("a", 64)}
	m.Kubernetes.Pinned, m.Kubernetes.Supported = pinned, supported
	m.Chart.Ref, m.Chart.Version = "oci://ghcr.io/ehilzinger/charts/kwerft", version
	return m
}

func testRepo() *fakeSource {
	return &fakeSource{
		releases: []Release{
			{Version: "0.8.0", Channel: ChannelStable},
			{Version: "0.7.0-rc.1", Channel: ChannelEdge},
			{Version: "0.6.0", Channel: ChannelStable},
			{Version: "0.5.1", Channel: ChannelStable},
			{Version: "0.5.0", Channel: ChannelStable},
			{Version: "0.4.0", Channel: ChannelStable},
		},
		manifests: map[string]*Manifest{
			"0.8.0":      manifest("0.8.0", "0.6.0", "v1.39.0+k3s1", "1.38", "1.39"),
			"0.7.0-rc.1": manifest("0.7.0-rc.1", "", "v1.38.1+k3s1", "1.37", "1.38"),
			"0.6.0":      manifest("0.6.0", "", "v1.38.1+k3s1", "1.37", "1.38"),
			"0.5.1":      manifest("0.5.1", "", "v1.37.2+k3s1", "1.36", "1.37"),
			"0.5.0":      manifest("0.5.0", "", "v1.37.2+k3s1", "1.36", "1.37"),
		},
		notes: map[string]string{"0.5.1": "Fixes.", "0.6.0": strings.Repeat("é", MaxNotes)},
	}
}

func find(av []kwerftv1.AvailableUpdate, c kwerftv1.UpgradeComponent, v string) *kwerftv1.AvailableUpdate {
	for i := range av {
		if av[i].Component == c && av[i].Version == v {
			return &av[i]
		}
	}
	return nil
}

func TestDiscoverOffersAllowedTargets(t *testing.T) {
	src := testRepo()
	d, err := Discover(context.Background(), src, Facts{Kwerft: "0.5.0", Kubernetes: "v1.37.1+k3s1",
		KubernetesMinors: []Version{MustVersion("v1.37.1+k3s1")}, Channel: ChannelStable})
	if err != nil {
		t.Fatal(err)
	}
	if find(d.Available, kwerftv1.UpgradeKwerft, "0.7.0-rc.1") != nil {
		t.Error("stable offered an edge release")
	}
	p := find(d.Available, kwerftv1.UpgradeKwerft, "0.5.1")
	if p == nil || p.Kind != "Patch" || !p.Allowed || p.Notes != "Fixes." {
		t.Errorf("0.5.1 = %+v", p)
	}
	m := find(d.Available, kwerftv1.UpgradeKwerft, "0.6.0")
	if m == nil || m.Kind != "Minor" || !m.Allowed || !strings.HasSuffix(m.Notes, "…") || len(m.Notes) > MaxNotes+8 {
		t.Errorf("0.6.0 = %+v", m)
	}
	// 0.8.0 upgrades from 0.6.0 only and does not support k3s 1.37.
	n := find(d.Available, kwerftv1.UpgradeKwerft, "0.8.0")
	if n == nil || n.Allowed || n.Reason != "Upgrade to 0.6.0 first." {
		t.Errorf("0.8.0 = %+v", n)
	}
	// k3s from the installed release's manifest only.
	k := find(d.Available, kwerftv1.UpgradeKubernetes, "v1.37.2+k3s1")
	if k == nil || k.Kind != "Patch" || !k.Allowed {
		t.Errorf("kubernetes = %+v (all: %+v)", k, d.Available)
	}
	if len(d.Available) != 4 {
		t.Errorf("available = %+v", d.Available)
	}
}

func TestDiscoverKubernetesFirst(t *testing.T) {
	src := testRepo()
	// On 0.6.0 with k3s 1.37: 0.8.0 is allowed by upgradeFrom, but supports
	// 1.38 and 1.39 only; the installed release pins 1.38 (a minor ahead).
	d, err := Discover(context.Background(), src, Facts{Kwerft: "0.6.0", Kubernetes: "v1.37.2+k3s1",
		KubernetesMinors: []Version{MustVersion("v1.37.2+k3s1")}, Channel: ChannelEdge})
	if err != nil {
		t.Fatal(err)
	}
	n := find(d.Available, kwerftv1.UpgradeKwerft, "0.8.0")
	if n == nil || n.Allowed || !strings.Contains(n.Reason, "upgrade Kubernetes first") {
		t.Errorf("0.8.0 = %+v", n)
	}
	if rc := find(d.Available, kwerftv1.UpgradeKwerft, "0.7.0-rc.1"); rc == nil || !rc.Allowed {
		t.Errorf("edge: 0.7.0-rc.1 = %+v", rc)
	}
	k := find(d.Available, kwerftv1.UpgradeKubernetes, "v1.38.1+k3s1")
	if k == nil || k.Kind != "Minor" {
		t.Errorf("kubernetes = %+v", k)
	}
}

func TestDiscoverNeverOffersTwoKubernetesMinors(t *testing.T) {
	src := testRepo()
	d, err := Discover(context.Background(), src, Facts{Kwerft: "0.6.0", Kubernetes: "v1.36.4+k3s1", Channel: ChannelStable})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Available {
		if a.Component == kwerftv1.UpgradeKubernetes {
			t.Errorf("offered %s two minors ahead", a.Version)
		}
	}
}

func TestDiscoverErrors(t *testing.T) {
	src := testRepo()
	src.err = errors.New("HTTP 503")
	if _, err := Discover(context.Background(), src, Facts{Kwerft: "0.5.0"}); err == nil {
		t.Error("no error when releases.json cannot be read")
	}
	src = testRepo()
	delete(src.manifests, "0.6.0")
	d, err := Discover(context.Background(), src, Facts{Kwerft: "0.5.0"})
	if err != nil || find(d.Available, kwerftv1.UpgradeKwerft, "0.6.0") != nil || find(d.Available, kwerftv1.UpgradeKwerft, "0.5.1") == nil {
		t.Errorf("a release without a manifest is skipped, the others offered: %+v %v", d, err)
	}
	if _, err := Discover(context.Background(), testRepo(), Facts{Kwerft: "abc"}); err == nil {
		t.Error("no error for a running version that is none")
	}
}

func TestManifestDefaults(t *testing.T) {
	m := manifest("0.6.0", "", "v1.38.1+k3s1")
	if from, _ := m.UpgradeFromVersion(); from.String() != "0.4.0" {
		t.Errorf("default upgradeFrom = %s, want two minors back", from)
	}
	if !m.IsRollbackSafe() || !m.Supports(MustVersion("v1.38.0+k3s1")) || m.Supports(MustVersion("v1.37.0+k3s1")) {
		t.Error("defaults: rollback-safe, supports the pinned minor only")
	}
}

func TestParseReleases(t *testing.T) {
	for _, body := range []string{
		`[{"version":"0.6.0","channel":"stable","published":"2026-11-02T10:00:00Z"},{"version":"nonsense"}]`,
		`{"releases":[{"version":"v0.6.0","channel":"stable"}]}`,
	} {
		rs, err := ParseReleases([]byte(body))
		if err != nil || len(rs) != 1 || rs[0].Version != "0.6.0" {
			t.Errorf("%s: %+v %v", body, rs, err)
		}
	}
	if _, err := ParseReleases([]byte(`"no"`)); err == nil {
		t.Error("parsed a string")
	}
}

func TestHTTPSource(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Cookie") != "" || r.URL.RawQuery != "" {
			t.Errorf("request carries more than a plain GET: %v", r.Header)
		}
		switch r.URL.Path {
		case "/main/releases.json":
			_, _ = w.Write([]byte(`[{"version":"0.6.0","channel":"stable"}]`))
		case "/main/v0.6.0/manifest.json":
			_, _ = w.Write([]byte(`{"version":"0.6.0","kubernetes":{"pinned":"v1.38.1+k3s1","supported":["1.37","1.38"]}}`))
		case "/main/v0.6.1/manifest.json":
			_, _ = w.Write([]byte(`{"version":"0.6.2"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src := &HTTPSource{BaseURL: srv.URL + "/main/", Client: srv.Client()}
	ctx := context.Background()
	if rs, err := src.Releases(ctx); err != nil || len(rs) != 1 {
		t.Fatalf("releases: %v %v", rs, err)
	}
	if m, err := src.Manifest(ctx, "v0.6.0"); err != nil || m.Kubernetes.Pinned != "v1.38.1+k3s1" {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if _, err := src.Manifest(ctx, "0.6.1"); err == nil {
		t.Error("accepted a manifest of another version")
	}
	if _, err := src.Notes(ctx, "0.6.0"); !errors.Is(err, ErrNotFound) {
		t.Errorf("notes: %v, want ErrNotFound", err)
	}
	if body, err := src.Fetch(ctx, "v0.6.0/manifest.json"); err != nil || !strings.Contains(string(body), "pinned") {
		t.Errorf("fetch: %v", err)
	}
}
