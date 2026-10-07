// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package oidctest is an in-process OpenID Connect provider for tests:
// discovery, signing keys, an authorization step without a browser and a
// token endpoint that checks the client, the redirect URI and PKCE.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// Issuer is a fake provider on an https:// test server.
type Issuer struct {
	URL                    string
	ClientID, ClientSecret string
	Now                    func() time.Time

	srv  *httptest.Server
	key  *rsa.PrivateKey
	kid  string
	mu   sync.Mutex
	code map[string]grant
	// Exchanges counts successful code exchanges.
	Exchanges int
}

type grant struct {
	clientID, redirect, challenge, nonce string
	claims                               map[string]any
	signWith                             *rsa.PrivateKey
}

// New starts a provider; it stops with the test.
func New(t *testing.T) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{ClientID: "kwerft-test", ClientSecret: "test-client-secret", key: key, kid: "key-1",
		code: map[string]grant{}, Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /keys", i.keys)
	mux.HandleFunc("POST /token", i.token)
	i.srv = httptest.NewTLSServer(mux)
	t.Cleanup(i.srv.Close)
	i.URL = i.srv.URL
	return i
}

// Client trusts the provider's certificate.
func (i *Issuer) Client() *http.Client { return i.srv.Client() }

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer": i.URL, "authorization_endpoint": i.URL + "/authorize", "token_endpoint": i.URL + "/token",
		"jwks_uri": i.URL + "/keys", "response_types_supported": []string{"code"},
		"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported": []string{"S256"},
	})
}

func (i *Issuer) keys(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &i.key.PublicKey, KeyID: i.kid, Algorithm: string(jose.RS256), Use: "sig"},
	}})
}

// Login is what the user does at the provider.
type Login struct {
	// Claims go into the ID token on top of iss, sub, aud, exp, iat and
	// nonce; they may override those too (to test a wrong audience, say).
	Claims map[string]any
	// ForeignKey signs the ID token with a key the provider never published.
	ForeignKey bool
}

// Authorize plays the browser at the provider: it reads the authorization
// URL the console redirected to and returns the callback URL with a code,
// as the provider would redirect back.
func (i *Issuer) Authorize(t *testing.T, authURL string, login Login) string {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme+"://"+u.Host != i.URL || u.Path != "/authorize" {
		t.Fatalf("authorization URL %s is not this provider's", authURL)
	}
	if q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" ||
		q.Get("state") == "" || q.Get("nonce") == "" || q.Get("client_id") != i.ClientID {
		t.Fatalf("authorization request lacks code flow, PKCE, state or nonce: %v", q)
	}
	g := grant{clientID: q.Get("client_id"), redirect: q.Get("redirect_uri"), challenge: q.Get("code_challenge"),
		nonce: q.Get("nonce"), claims: login.Claims}
	if login.ForeignKey {
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		g.signWith = other
	}
	code := randomString()
	i.mu.Lock()
	i.code[code] = g
	i.mu.Unlock()
	back, err := url.Parse(g.redirect)
	if err != nil {
		t.Fatal(err)
	}
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	return back.String()
}

func randomString() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	fail := func(code int, e string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": e})
	}
	if err := r.ParseForm(); err != nil {
		fail(http.StatusBadRequest, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if ok {
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
	} else {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != i.ClientID || secret != i.ClientSecret {
		fail(http.StatusUnauthorized, "invalid_client")
		return
	}
	i.mu.Lock()
	g, found := i.code[r.PostForm.Get("code")]
	delete(i.code, r.PostForm.Get("code")) // single use
	i.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	switch {
	case r.PostForm.Get("grant_type") != "authorization_code" || !found:
		fail(http.StatusBadRequest, "invalid_grant")
		return
	case g.redirect != r.PostForm.Get("redirect_uri") || g.clientID != id:
		fail(http.StatusBadRequest, "invalid_grant")
		return
	case base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge:
		fail(http.StatusBadRequest, "invalid_grant") // PKCE
		return
	}
	now := i.Now()
	claims := map[string]any{"iss": i.URL, "sub": "subject-1", "aud": i.ClientID, "iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(), "nonce": g.nonce}
	for k, v := range g.claims {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	key := i.key
	if g.signWith != nil {
		key = g.signWith
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: i.kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		fail(http.StatusInternalServerError, "server_error")
		return
	}
	payload, _ := json.Marshal(claims)
	jws, err := signer.Sign(payload)
	if err != nil {
		fail(http.StatusInternalServerError, "server_error")
		return
	}
	raw, _ := jws.CompactSerialize()
	i.mu.Lock()
	i.Exchanges++
	i.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-" + randomString(), "token_type": "Bearer",
		"expires_in": 300, "id_token": raw})
}
