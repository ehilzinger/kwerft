// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package jointoken

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestIssueVerify(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s := NewSigner([]byte("0123456789abcdef0123456789abcdef"))
	s.Now = func() time.Time { return now }

	tok, c, err := s.Issue(Claims{Cluster: "prod", Role: RoleWorker, Node: "prod-w-abcde"}, time.Hour)
	if err != nil || !strings.HasPrefix(tok, Prefix) || c.ID == "" {
		t.Fatalf("issue: %q %+v %v", tok, c, err)
	}
	got, err := s.Verify(tok)
	if err != nil || got.Cluster != "prod" || got.Node != "prod-w-abcde" || got.Role != RoleWorker {
		t.Fatalf("verify: %+v %v", got, err)
	}

	// Another console's key, a changed payload and junk are invalid.
	other := NewSigner([]byte("another key, another console....."))
	if _, err := other.Verify(tok); !errors.Is(err, ErrInvalid) {
		t.Fatalf("other key: %v", err)
	}
	p, sig, _ := strings.Cut(strings.TrimPrefix(tok, Prefix), ".")
	if _, err := s.Verify(Prefix + p + "x." + sig); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered: %v", err)
	}
	for _, junk := range []string{"", "kwft_join_", "kwft_join_a.b", "Bearer x", strings.Repeat("a", 2000)} {
		if _, err := s.Verify(junk); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%q: %v", junk, err)
		}
	}

	now = now.Add(time.Hour)
	if _, err := s.Verify(tok); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestIssueLimits(t *testing.T) {
	s := NewSigner([]byte("k"))
	if _, _, err := s.Issue(Claims{Cluster: "c", Role: "admin"}, time.Hour); err == nil {
		t.Fatal("unknown role accepted")
	}
	if _, _, err := s.Issue(Claims{Cluster: "c", Role: RoleWorker}, 48*time.Hour); err == nil {
		t.Fatal("ttl over 24 h accepted")
	}
	var none *Signer
	if _, _, err := none.Issue(Claims{Cluster: "c", Role: RoleWorker}, time.Hour); err == nil {
		t.Fatal("nil signer issued")
	}
	if NewSigner(nil) != nil {
		t.Fatal("signer without a key")
	}
}

func TestBootstrapToken(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	bt, err := NewBootstrapToken("K10abc123::server:s3cret", "kwerft join", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	id := bt.Secret.StringData["token-id"]
	if bt.Secret.Name != "bootstrap-token-"+id || bt.Secret.Namespace != "kube-system" || len(id) != 6 || len(bt.Secret.StringData["token-secret"]) != 16 {
		t.Fatalf("secret = %+v", bt.Secret)
	}
	if bt.AgentToken != "K10abc123::"+id+"."+bt.Secret.StringData["token-secret"] {
		t.Fatalf("agent token = %q", bt.AgentToken)
	}
	if bt.Secret.StringData["expiration"] != "2026-10-05T13:00:00Z" {
		t.Fatalf("expiration = %s", bt.Secret.StringData["expiration"])
	}
	// Without a K10 server token there is no CA hash to keep.
	bt, _ = NewBootstrapToken("plain", "", time.Hour, now)
	if strings.Contains(bt.AgentToken, "::") {
		t.Fatalf("agent token = %q", bt.AgentToken)
	}
}
