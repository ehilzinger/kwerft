// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package upgrades

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRegistry answers like ghcr.io: 401 with a Bearer challenge, an
// anonymous token from the realm, then the manifest for public repositories.
func fakeRegistry(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			if r.Header.Get("Authorization") != "" {
				t.Error("token request carries credentials")
			}
			if !strings.HasPrefix(r.URL.Query().Get("scope"), "repository:ehilzinger/") || r.URL.Query().Get("service") != "reg" {
				t.Errorf("token query %s", r.URL.RawQuery)
			}
			if strings.Contains(r.URL.Query().Get("scope"), "private") {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"token":"anon"}`))
		case r.Header.Get("Authorization") != "Bearer anon":
			w.Header().Set("WWW-Authenticate", `Bearer realm="`+srv.URL+`/token",service="reg",scope="repository:x:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/v2/ehilzinger/kwerft/manifests/sha256:abc", r.URL.Path == "/v2/ehilzinger/charts/kwerft/manifests/0.6.0":
			if r.Method != http.MethodHead {
				t.Errorf("method %s", r.Method)
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOCIRegistryPullable(t *testing.T) {
	srv := fakeRegistry(t)
	host := strings.TrimPrefix(srv.URL, "http://")
	reg := &OCIRegistry{Client: srv.Client(), PlainHTTP: true}
	ctx := context.Background()
	for ref, want := range map[string]string{
		host + "/ehilzinger/kwerft@sha256:abc":              "",
		"oci://" + host + "/ehilzinger/charts/kwerft:0.6.0": "",
		host + "/ehilzinger/kwerft:9.9.9":                   "is not published",
		host + "/ehilzinger/private:1.0.0":                  "not pullable anonymously",
	} {
		err := reg.Pullable(ctx, ref)
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", ref, err)
		case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
			t.Errorf("%s: %v, want %q", ref, err, want)
		}
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][3]string{
		"ghcr.io/ehilzinger/kwerft:0.6.0":          {"ghcr.io", "ehilzinger/kwerft", "0.6.0"},
		"ghcr.io/ehilzinger/kwerft@sha256:abc":     {"ghcr.io", "ehilzinger/kwerft", "sha256:abc"},
		"oci://ghcr.io/ehilzinger/charts/kwerft:1": {"ghcr.io", "ehilzinger/charts/kwerft", "1"},
		"localhost:5000/kwerft:dev":                {"localhost:5000", "kwerft", "dev"},
	} {
		h, r, ref2, err := SplitRef(ref)
		if err != nil || [3]string{h, r, ref2} != want {
			t.Errorf("%s: %s %s %s %v", ref, h, r, ref2, err)
		}
	}
	for _, bad := range []string{"kwerft", "ghcr.io/kwerft", "localhost:5000/kwerft"} {
		if _, _, _, err := SplitRef(bad); err == nil {
			t.Errorf("%s split", bad)
		}
	}
}

func TestDigestRef(t *testing.T) {
	d := "sha256:" + strings.Repeat("b", 64)
	for _, c := range []struct{ image, id, want string }{
		{"ghcr.io/ehilzinger/kwerft:0.5.0", "ghcr.io/ehilzinger/kwerft@" + d, "ghcr.io/ehilzinger/kwerft@" + d},
		{"ghcr.io/ehilzinger/kwerft:0.5.0", "docker-pullable://ghcr.io/ehilzinger/kwerft@" + d, "ghcr.io/ehilzinger/kwerft@" + d},
		{"localhost:5000/kwerft:dev", "localhost:5000/kwerft@" + d, "localhost:5000/kwerft@" + d},
	} {
		if got, err := DigestRef(c.image, c.id); err != nil || got != c.want {
			t.Errorf("%s %s: %s %v", c.image, c.id, got, err)
		}
	}
	if _, err := DigestRef("ghcr.io/ehilzinger/kwerft:dev", d); err == nil {
		t.Error("pinned an imported image without a registry digest")
	}
}

func TestParseChallenge(t *testing.T) {
	p := parseChallenge(`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:a/b:pull"`)
	if p["realm"] != "https://ghcr.io/token" || p["service"] != "ghcr.io" || p["scope"] != "repository:a/b:pull" {
		t.Errorf("challenge = %v", p)
	}
	if len(parseChallenge(`Basic realm="x"`)) != 0 {
		t.Error("parsed a Basic challenge")
	}
}
