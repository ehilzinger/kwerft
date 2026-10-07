// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Registry tells whether an image or chart can be pulled anonymously: the
// installer's exit-50 check (check_release), before anything changes.
type Registry interface {
	Pullable(ctx context.Context, ref string) error
}

// OCIRegistry asks the registry over the distribution API, with an
// anonymous bearer token where the registry wants one (ghcr.io does).
type OCIRegistry struct {
	Client *http.Client
	// PlainHTTP talks http:// to registries (tests).
	PlainHTTP bool
}

const manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"

// SplitRef splits host/repo:tag, host/repo@sha256:… or oci://host/repo:tag
// into host, repository and reference.
func SplitRef(ref string) (host, repo, reference string, err error) {
	r := strings.TrimPrefix(ref, "oci://")
	slash := strings.IndexByte(r, '/')
	if slash <= 0 {
		return "", "", "", fmt.Errorf("reference %q has no registry host", ref)
	}
	host, rest := r[:slash], r[slash+1:]
	if at := strings.IndexByte(rest, '@'); at >= 0 {
		repo, reference = rest[:at], rest[at+1:]
	} else if colon := strings.LastIndexByte(rest, ':'); colon > strings.LastIndexByte(rest, '/') {
		repo, reference = rest[:colon], rest[colon+1:]
	}
	if repo == "" || reference == "" {
		return "", "", "", fmt.Errorf("reference %q needs a tag or digest", ref)
	}
	return host, repo, reference, nil
}

// DigestRef pins image to the digest the node pulled: imageID as a pod's
// container status reports it (repository@sha256:…, possibly with a
// docker-pullable:// prefix). A bare "sha256:…" is a locally imported
// image without a registry digest, which another pod cannot pull.
func DigestRef(image, imageID string) (string, error) {
	id := strings.TrimPrefix(imageID, "docker-pullable://")
	at := strings.Index(id, "@sha256:")
	if at <= 0 {
		return "", fmt.Errorf("image %s has no registry digest (%q): a locally imported image", image, imageID)
	}
	// The repository as the Deployment names it, the digest as pulled.
	repo := image
	if i := strings.IndexByte(repo, '@'); i >= 0 {
		repo = repo[:i]
	} else if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
		repo = repo[:i]
	}
	if repo == "" {
		repo = id[:at]
	}
	return repo + id[at:], nil
}

func (o *OCIRegistry) client() *http.Client {
	if o.Client != nil {
		return o.Client
	}
	return http.DefaultClient
}

func (o *OCIRegistry) Pullable(ctx context.Context, ref string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	host, repo, reference, err := SplitRef(ref)
	if err != nil {
		return err
	}
	scheme := "https"
	if o.PlainHTTP {
		scheme = "http"
	}
	manifest := fmt.Sprintf("%s://%s/v2/%s/manifests/%s", scheme, host, repo, reference)
	code, challenge, err := o.head(ctx, manifest, "")
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", host, err)
	}
	if code == http.StatusUnauthorized && challenge != "" {
		token, terr := o.anonymousToken(ctx, challenge, repo)
		if terr != nil {
			return fmt.Errorf("%s is not pullable anonymously: %w", ref, terr)
		}
		if code, _, err = o.head(ctx, manifest, token); err != nil {
			return fmt.Errorf("cannot reach %s: %w", host, err)
		}
	}
	switch code {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
		return fmt.Errorf("%s is not published", ref)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%s is not pullable anonymously (HTTP %d)", ref, code)
	default:
		return fmt.Errorf("unexpected HTTP %d from %s for %s", code, host, ref)
	}
}

func (o *OCIRegistry) head(ctx context.Context, u, token string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", manifestAccept)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return 0, "", err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("WWW-Authenticate"), nil
}

// anonymousToken follows a Bearer challenge without credentials.
func (o *OCIRegistry) anonymousToken(ctx context.Context, challenge, repo string) (string, error) {
	params := parseChallenge(challenge)
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("registry challenge without realm")
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	q.Set("scope", "repository:"+repo+":pull")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint answered HTTP %d", resp.StatusCode)
	}
	var tok struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&tok); err != nil {
		return "", err
	}
	if tok.Token == "" {
		tok.Token = tok.AccessToken
	}
	if tok.Token == "" {
		return "", fmt.Errorf("token endpoint returned no token")
	}
	return tok.Token, nil
}

// parseChallenge reads `Bearer realm="…",service="…",scope="…"`.
func parseChallenge(h string) map[string]string {
	out := map[string]string{}
	scheme, rest, ok := strings.Cut(strings.TrimSpace(h), " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return out
	}
	for rest != "" {
		rest = strings.TrimLeft(rest, ", ")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		var val string
		if strings.HasPrefix(after, `"`) {
			end := strings.IndexByte(after[1:], '"')
			if end < 0 {
				val, rest = after[1:], ""
			} else {
				val, rest = after[1:1+end], after[2+end:]
			}
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		out[strings.ToLower(strings.TrimSpace(key))] = val
	}
	return out
}
