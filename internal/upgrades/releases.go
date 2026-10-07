// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
)

// DefaultInstallBaseURL is the public install repository's raw files
// (ehilzinger/kwerft-install, branch main): releases.json at the top,
// v<version>/{install.sh,join.sh,SHA256SUMS,manifest.json,NOTES.md}.
const DefaultInstallBaseURL = "https://raw.githubusercontent.com/ehilzinger/kwerft-install/main"

// Channels.
const (
	ChannelStable = "stable"
	ChannelEdge   = "edge"
)

// Release is one entry of releases.json.
type Release struct {
	Version   string    `json:"version"`
	Channel   string    `json:"channel"`
	Published time.Time `json:"published"`
}

// Manifest is v<version>/manifest.json, written by hack/release.sh.
type Manifest struct {
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	Published   string `json:"published,omitempty"`
	UpgradeFrom string `json:"upgradeFrom,omitempty"`
	// RollbackSafe defaults to true when absent.
	RollbackSafe *bool `json:"rollbackSafe,omitempty"`
	Kubernetes   struct {
		Pinned    string   `json:"pinned"`
		Supported []string `json:"supported"`
	} `json:"kubernetes"`
	Components map[string]string `json:"components,omitempty"`
	// Image is the console image by digest (repository@sha256:…).
	Image string `json:"image"`
	Chart struct {
		Ref     string `json:"ref"`
		Version string `json:"version"`
		Digest  string `json:"digest,omitempty"`
	} `json:"chart"`
}

// IsRollbackSafe: absent means true.
func (m *Manifest) IsRollbackSafe() bool { return m.RollbackSafe == nil || *m.RollbackSafe }

// Supports reports whether the release supports a k3s version (its minor is
// in kubernetes.supported; an empty list supports the pinned minor only).
func (m *Manifest) Supports(k3s Version) bool {
	supported := m.Kubernetes.Supported
	if len(supported) == 0 {
		if p, err := ParseVersion(m.Kubernetes.Pinned); err == nil {
			supported = []string{p.MinorString()}
		}
	}
	return slices.Contains(supported, k3s.MinorString())
}

// UpgradeFromVersion is the oldest release that may upgrade to m: its
// upgradeFrom, or by default two minor releases back.
func (m *Manifest) UpgradeFromVersion() (Version, error) {
	if m.UpgradeFrom != "" {
		return ParseVersion(m.UpgradeFrom)
	}
	v, err := ParseVersion(m.Version)
	if err != nil {
		return Version{}, err
	}
	minor := max(v.Minor-2, 0)
	return Version{Major: v.Major, Minor: minor, raw: fmt.Sprintf("%d.%d.0", v.Major, minor)}, nil
}

// OnChannel: the stable channel sees stable releases, edge sees every
// release.
func OnChannel(releaseChannel, channel string) bool {
	if channel == ChannelEdge {
		return true
	}
	return releaseChannel == "" || releaseChannel == ChannelStable
}

// Source reads the install repository. Only plain GETs, nothing that
// identifies the console (docs/phase6-upgrades.md › Security).
type Source interface {
	Releases(ctx context.Context) ([]Release, error)
	Manifest(ctx context.Context, version string) (*Manifest, error)
	Notes(ctx context.Context, version string) (string, error)
}

// HTTPSource is the install repository over HTTP(S).
type HTTPSource struct {
	// BaseURL defaults to DefaultInstallBaseURL.
	BaseURL string
	Client  *http.Client
}

// ErrNotFound: the file does not exist (a release without notes).
var ErrNotFound = errors.New("not found")

// maxFile bounds what is read from the repository.
const maxFile = 1 << 20

func (s *HTTPSource) base() string {
	if s.BaseURL == "" {
		return DefaultInstallBaseURL
	}
	return strings.TrimRight(s.BaseURL, "/")
}

// URL is a file of the install repository.
func (s *HTTPSource) URL(path string) string { return s.base() + "/" + strings.TrimLeft(path, "/") }

func (s *HTTPSource) get(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL(path), nil)
	if err != nil {
		return nil, err
	}
	c := s.Client
	if c == nil {
		c = http.DefaultClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", path, ErrNotFound)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFile+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxFile {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxFile)
	}
	return body, nil
}

func (s *HTTPSource) Releases(ctx context.Context) ([]Release, error) {
	body, err := s.get(ctx, "releases.json")
	if err != nil {
		return nil, err
	}
	return ParseReleases(body)
}

func (s *HTTPSource) Manifest(ctx context.Context, version string) (*Manifest, error) {
	body, err := s.get(ctx, "v"+strings.TrimPrefix(version, "v")+"/manifest.json")
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest of %s: %w", version, err)
	}
	if m.Version != strings.TrimPrefix(version, "v") {
		return nil, fmt.Errorf("manifest of %s names version %q", version, m.Version)
	}
	return &m, nil
}

func (s *HTTPSource) Notes(ctx context.Context, version string) (string, error) {
	body, err := s.get(ctx, "v"+strings.TrimPrefix(version, "v")+"/NOTES.md")
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ParseReleases reads releases.json: a list newest first, or an object
// with the list under "releases". Entries that are no version are dropped.
func ParseReleases(body []byte) ([]Release, error) {
	var list []Release
	if err := json.Unmarshal(body, &list); err != nil {
		var obj struct {
			Releases []Release `json:"releases"`
		}
		if err2 := json.Unmarshal(body, &obj); err2 != nil {
			return nil, fmt.Errorf("releases.json: %w", err)
		}
		list = obj.Releases
	}
	out := list[:0]
	for _, r := range list {
		if _, err := ParseVersion(r.Version); err == nil {
			r.Version = strings.TrimPrefix(r.Version, "v")
			out = append(out, r)
		}
	}
	return out, nil
}

// MaxNotes bounds the release notes kept in ConsoleSettings' status.
const MaxNotes = 8 << 10

// MaxKwerftTargets bounds how many newer releases discovery reads.
const MaxKwerftTargets = 12

// Facts are what discovery knows about the cluster.
type Facts struct {
	Kwerft string
	// Kubernetes is the oldest kubelet version among the nodes.
	Kubernetes string
	// KubernetesMinors are the k3s minors the nodes run (all of them must
	// be supported by a Kwerft target).
	KubernetesMinors []Version
	Channel          string
}

// Discovery is what a check found.
type Discovery struct {
	Available []kwerftv1.AvailableUpdate
	// Manifests of the releases read, by version (the installed one too,
	// when it could be read).
	Manifests map[string]*Manifest
}

// Discover reads releases.json and the manifests of newer releases on the
// channel, and decides which targets are offered (docs/phase6-upgrades.md ›
// Which targets are offered). Notes are read best-effort.
func Discover(ctx context.Context, src Source, f Facts) (*Discovery, error) {
	cur, err := ParseVersion(f.Kwerft)
	if err != nil {
		return nil, fmt.Errorf("running version %q: %w", f.Kwerft, err)
	}
	releases, err := src.Releases(ctx)
	if err != nil {
		return nil, err
	}
	d := &Discovery{Manifests: map[string]*Manifest{}}
	var newer []Version
	for _, r := range releases {
		v, _ := ParseVersion(r.Version)
		if cur.Less(v) && OnChannel(r.Channel, f.Channel) {
			newer = append(newer, v)
		}
	}
	slices.SortFunc(newer, func(a, b Version) int { return b.Compare(a) }) // newest first
	if len(newer) > MaxKwerftTargets {
		newer = newer[:MaxKwerftTargets]
	}
	kept := newer[:0]
	for _, v := range newer {
		m, err := src.Manifest(ctx, v.String())
		if errors.Is(err, ErrNotFound) {
			continue // releases before manifests (≤ 0.5.x) cannot be upgraded to from the console
		}
		if err != nil {
			return nil, err
		}
		d.Manifests[m.Version] = m
		kept = append(kept, v)
	}
	newer = kept
	if m, err := src.Manifest(ctx, cur.String()); err == nil {
		d.Manifests[m.Version] = m
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	// The newest release whose upgradeFrom allows the running version: the
	// step to take when a newer one does not.
	var step string
	for _, v := range newer {
		if ok, _ := upgradeFromAllows(d.Manifests[v.String()], cur); ok {
			step = v.String()
			break
		}
	}
	for _, v := range newer {
		m := d.Manifests[v.String()]
		u := kwerftv1.AvailableUpdate{Component: kwerftv1.UpgradeKwerft, Version: v.String(), Kind: UpgradeKind(cur, v), Allowed: true}
		u.Allowed, u.Reason = KwerftAllowed(m, cur, f.KubernetesMinors)
		if !u.Allowed && step != "" && u.Reason == reasonUpgradeFrom(m) {
			u.Reason = fmt.Sprintf("Upgrade to %s first.", step)
		}
		d.Available = append(d.Available, u)
	}
	if k := kubernetesTarget(d.Manifests[cur.String()], f.Kubernetes); k != nil {
		d.Available = append(d.Available, *k)
	}
	for i := range d.Available {
		a := &d.Available[i]
		if a.Component != kwerftv1.UpgradeKwerft {
			continue
		}
		if notes, err := src.Notes(ctx, a.Version); err == nil {
			a.Notes = truncate(notes, MaxNotes)
		}
	}
	return d, nil
}

func reasonUpgradeFrom(m *Manifest) string {
	from, _ := m.UpgradeFromVersion()
	return fmt.Sprintf("%s upgrades from %s or newer.", m.Version, from)
}

func upgradeFromAllows(m *Manifest, cur Version) (bool, string) {
	from, err := m.UpgradeFromVersion()
	if err != nil {
		return false, fmt.Sprintf("manifest of %s: upgradeFrom: %v", m.Version, err)
	}
	if cur.Less(from) {
		return false, reasonUpgradeFrom(m)
	}
	return true, ""
}

// KwerftAllowed is the rule part of a Kwerft target (no cluster state
// beyond the k3s minors): upgradeFrom allows the running release, and the
// release supports every k3s minor running. Newer-ness and the channel are
// the caller's.
func KwerftAllowed(m *Manifest, cur Version, k3sMinors []Version) (bool, string) {
	if ok, reason := upgradeFromAllows(m, cur); !ok {
		return false, reason
	}
	for _, k := range k3sMinors {
		if !m.Supports(k) {
			return false, fmt.Sprintf("%s supports Kubernetes %s; this cluster runs %s: upgrade Kubernetes first.",
				m.Version, strings.Join(m.Kubernetes.Supported, " and "), k.MinorString())
		}
	}
	return true, ""
}

// KubernetesTarget is the k3s version offered to a cluster that runs the
// Kwerft release of installed (its manifest) and Kubernetes running, or
// nil: an agent cluster's, which discovery does not cover.
func KubernetesTarget(installed *Manifest, running string) *kwerftv1.AvailableUpdate {
	return kubernetesTarget(installed, running)
}

// kubernetesTarget: the installed release's pinned k3s, when it is newer
// than the running version and at most one minor ahead. k3s versions come
// only from Kwerft manifests, never from k3s's channel server.
func kubernetesTarget(installed *Manifest, running string) *kwerftv1.AvailableUpdate {
	if installed == nil || installed.Kubernetes.Pinned == "" || running == "" {
		return nil
	}
	pin, err := ParseVersion(installed.Kubernetes.Pinned)
	if err != nil {
		return nil
	}
	run, err := ParseVersion(running)
	if err != nil || !run.Less(pin) || pin.MinorsAhead(run) > 1 {
		return nil
	}
	return &kwerftv1.AvailableUpdate{Component: kwerftv1.UpgradeKubernetes, Version: pin.String(), Kind: UpgradeKind(run, pin), Allowed: true}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	// Do not cut a UTF-8 sequence in half.
	for len(s) > 0 && s[len(s)-1]&0xC0 == 0x80 {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] >= 0xC0 {
		s = s[:len(s)-1]
	}
	return s + "\n…"
}
