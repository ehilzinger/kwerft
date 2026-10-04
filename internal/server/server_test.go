package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	ui := fstest.MapFS{
		"index.html":         {Data: []byte("<!doctype html><title>Werft</title>")},
		"assets/app-1a2b.js": {Data: []byte("console.log(1)")},
	}
	return Handler(Config{Platform: "cloud", UI: ui, Logger: slog.New(slog.DiscardHandler)})
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

func TestHealth(t *testing.T) {
	for _, p := range []string{"/healthz", "/readyz"} {
		if got := get(t, testHandler(), p).Code; got != http.StatusOK {
			t.Errorf("%s: status %d, want 200", p, got)
		}
	}
}

func TestReadyzWaitsForCaches(t *testing.T) {
	ready := false
	h := Handler(Config{UI: fstest.MapFS{}, Logger: slog.New(slog.DiscardHandler), Ready: func() bool { return ready }})
	if got := get(t, h, "/readyz").Code; got != http.StatusServiceUnavailable {
		t.Errorf("before sync: status %d, want 503", got)
	}
	ready = true
	if got := get(t, h, "/readyz").Code; got != http.StatusOK {
		t.Errorf("after sync: status %d, want 200", got)
	}
}

func TestVersion(t *testing.T) {
	rec := get(t, testHandler(), "/api/v1/version")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["platform"] != "cloud" || body["version"] == "" {
		t.Errorf("unexpected body %v", body)
	}
}

func TestUnknownAPIPathIsJSON404(t *testing.T) {
	rec := get(t, testHandler(), "/api/v1/nope")
	if rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("got %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestSPAFallbackServesIndexForRoutes(t *testing.T) {
	rec := get(t, testHandler(), "/apps/storefront/api")
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != http.StatusOK || string(body) != "<!doctype html><title>Werft</title>" {
		t.Errorf("got %d %q", rec.Code, body)
	}
}

func TestMissingAssetIs404(t *testing.T) {
	if got := get(t, testHandler(), "/assets/missing.js").Code; got != http.StatusNotFound {
		t.Errorf("status %d, want 404", got)
	}
}

func TestAssetsAreImmutable(t *testing.T) {
	rec := get(t, testHandler(), "/assets/app-1a2b.js")
	if rec.Code != http.StatusOK || rec.Header().Get("Cache-Control") == "" {
		t.Errorf("got %d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestSecurityHeaders(t *testing.T) {
	rec := get(t, testHandler(), "/")
	if rec.Header().Get("Content-Security-Policy") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("missing security headers: %v", rec.Header())
	}
}
