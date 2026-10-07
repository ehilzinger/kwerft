// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"encoding/base64"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B, SHA-1 rows: the reference secret is the ASCII string
// "12345678901234567890" and the expected values have 8 digits.
func TestTOTPRFC6238Vectors(t *testing.T) {
	secret := []byte("12345678901234567890")
	for _, v := range []struct {
		unix int64
		want string
	}{
		{59, "94287082"},
		{1111111109, "07081804"},
		{1111111111, "14050471"},
		{1234567890, "89005924"},
		{2000000000, "69279037"},
		{20000000000, "65353130"},
	} {
		at := time.Unix(v.unix, 0)
		if got := hotp(secret, TOTPStep(at), 8); got != v.want {
			t.Errorf("T=%d: got %s, want %s", v.unix, got, v.want)
		}
		// Six digits are the same value modulo 10^6.
		if got := TOTPCode(secret, at); got != v.want[2:] {
			t.Errorf("T=%d 6 digits: got %s, want %s", v.unix, got, v.want[2:])
		}
	}
}

func TestMatchTOTPAcceptsOneStepOfDrift(t *testing.T) {
	secret := NewTOTPSecret()
	now := time.Unix(1_800_000_000, 0)
	for _, d := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		code := TOTPCode(secret, now.Add(d))
		step, ok := MatchTOTP(secret, code[:3]+" "+code[3:], now)
		if !ok || step != TOTPStep(now.Add(d)) {
			t.Errorf("drift %v: ok=%v step=%d", d, ok, step)
		}
	}
	for _, d := range []time.Duration{-61 * time.Second, 61 * time.Second} {
		if _, ok := MatchTOTP(secret, TOTPCode(secret, now.Add(d)), now); ok {
			t.Errorf("drift %v accepted", d)
		}
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := MatchTOTP(secret, bad, now); ok {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestTOTPURIAndSecretText(t *testing.T) {
	secret := []byte("12345678901234567890")
	u, err := url.Parse(TOTPURI("Kwerft ops.example.com", "mara@example.com", secret))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "otpauth" || u.Host != "totp" || u.Path != "/Kwerft ops.example.com:mara@example.com" {
		t.Errorf("uri = %s", u)
	}
	q := u.Query()
	if q.Get("secret") != "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" || q.Get("digits") != "6" || q.Get("period") != "30" || q.Get("issuer") != "Kwerft ops.example.com" {
		t.Errorf("query = %v", q)
	}
	if got := TOTPSecretText(secret); got != "GEZD GNBV GY3T QOJQ GEZD GNBV GY3T QOJQ" {
		t.Errorf("secret text = %q", got)
	}
	if !LooksLikeTOTP(" 123 456 ") || LooksLikeTOTP("correct horse battery") || LooksLikeTOTP("12345a") {
		t.Error("LooksLikeTOTP misclassifies")
	}
}

func TestRecoveryCodes(t *testing.T) {
	codes := NewRecoveryCodes(RecoveryCodeCount)
	seen := map[string]bool{}
	for _, c := range codes {
		if len(c) != 19 || strings.Count(c, "-") != 3 || strings.ContainsAny(c, "ilou") {
			t.Errorf("bad code %q", c)
		}
		if seen[c] {
			t.Errorf("duplicate %q", c)
		}
		seen[c] = true
	}
	c := codes[0]
	if HashRecoveryCode(c) != HashRecoveryCode(" "+strings.ToUpper(strings.ReplaceAll(c, "-", ""))+" ") {
		t.Error("hash must ignore case, dashes and spaces")
	}
	if HashRecoveryCode("0000-1111") != HashRecoveryCode("oooo-iiLl") {
		t.Error("look-alike letters should map to digits")
	}
	if HashRecoveryCode(codes[0]) == HashRecoveryCode(codes[1]) {
		t.Error("different codes hash the same")
	}
}

func TestSealerRoundTripAndWrongKey(t *testing.T) {
	key, err := ParseDataKey(NewDataKey())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSealer(key)
	if err != nil {
		t.Fatal(err)
	}
	sealed := s.Seal([]byte("totp seed"), "totp:user1")
	if !strings.HasPrefix(sealed, "v2:"+KeyID(key)+":") || strings.Contains(sealed, "totp seed") {
		t.Errorf("sealed = %q", sealed)
	}
	if s.Seal([]byte("totp seed"), "totp:user1") == sealed {
		t.Error("nonce must make every seal different")
	}
	if pt, err := s.Open(sealed, "totp:user1"); err != nil || string(pt) != "totp seed" {
		t.Errorf("open: %q %v", pt, err)
	}
	if _, err := s.Open(sealed, "totp:user2"); !errors.Is(err, ErrSealed) {
		t.Errorf("other context: %v, want ErrSealed", err)
	}
	other, _ := ParseDataKey(NewDataKey())
	s2, _ := NewSealer(other)
	if _, err := s2.Open(sealed, "totp:user1"); !errors.Is(err, ErrSealed) {
		t.Errorf("wrong key: %v, want ErrSealed", err)
	}
	body := sealed[len("v2:"+KeyID(key)+":"):]
	for _, bad := range []string{"", "v1:", "v1:!!!", "v2:", "v2:" + body, "v2:00000000:" + body, "v3:" + sealed[3:], sealed[:len(sealed)-2]} {
		if _, err := s.Open(bad, "totp:user1"); !errors.Is(err, ErrSealed) {
			t.Errorf("Open(%q): %v, want ErrSealed", bad, err)
		}
	}
}

// sealV1 seals the way Kwerft 0.1 did: "v1:" and no key ID.
func sealV1(t *testing.T, key []byte, pt, context string) string {
	t.Helper()
	k, err := newSealKey(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, k.aead.NonceSize())
	out := k.aead.Seal(nonce, nonce, []byte(pt), []byte(context))
	return "v1:" + base64.RawStdEncoding.EncodeToString(out)
}

func TestSealerRotation(t *testing.T) {
	k1, _ := ParseDataKey(NewDataKey())
	k2, _ := ParseDataKey(NewDataKey())
	k3, _ := ParseDataKey(NewDataKey())
	old, _ := NewSealer(k1)
	v1 := sealV1(t, k1, "seed-a", "totp:a")
	v2 := old.Seal([]byte("seed-b"), "totp:b")

	// The upgrade path: a 0.1 value opens with the same key and reseals to v2.
	if pt, err := old.Open(v1, "totp:a"); err != nil || string(pt) != "seed-a" {
		t.Fatalf("v1 value: %q %v", pt, err)
	}
	if old.Current(v1) || !old.Current(v2) {
		t.Errorf("Current: v1 %v, v2 %v", old.Current(v1), old.Current(v2))
	}

	// After a restart with KWERFT_DATA_KEY=k2 and the previous key k1.
	s, err := NewSealer(k2, k1)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.KeyIDs(); len(got) != 2 || got[0] != KeyID(k2) || got[1] != KeyID(k1) || s.PrimaryID() != KeyID(k2) {
		t.Errorf("key IDs %v, primary %s", got, s.PrimaryID())
	}
	for sealed, want := range map[string]string{v1: "seed-a", v2: "seed-b"} {
		ctx := "totp:a"
		if want == "seed-b" {
			ctx = "totp:b"
		}
		out, changed, err := s.Reseal(sealed, ctx)
		if err != nil || !changed || !s.Current(out) {
			t.Fatalf("reseal %q: %q %v %v", want, out, changed, err)
		}
		if again, changed, _ := s.Reseal(out, ctx); changed || again != out {
			t.Error("resealing a current value changed it")
		}
		// The new key alone opens the resealed value.
		only, _ := NewSealer(k2)
		if pt, err := only.Open(out, ctx); err != nil || string(pt) != want {
			t.Errorf("open with the new key alone: %q %v", pt, err)
		}
	}
	if _, _, err := s.Reseal(v2, "totp:other"); !errors.Is(err, ErrSealed) {
		t.Errorf("reseal with a wrong context: %v", err)
	}

	// Rotating in place: k3 seals, k2 and k1 still open until forgotten.
	if err := s.Rotate(k3); err != nil {
		t.Fatal(err)
	}
	if s.PrimaryID() != KeyID(k3) || len(s.KeyIDs()) != 3 {
		t.Errorf("after rotate: %s %v", s.PrimaryID(), s.KeyIDs())
	}
	if _, err := s.Open(v2, "totp:b"); err != nil {
		t.Errorf("old value after rotate: %v", err)
	}
	s.Forget()
	if _, err := s.Open(v2, "totp:b"); !errors.Is(err, ErrSealed) {
		t.Errorf("old value after forget: %v", err)
	}
	if ids := s.KeyIDs(); len(ids) != 1 || ids[0] != KeyID(k3) {
		t.Errorf("after forget: %v", ids)
	}
	if _, err := NewSealer(k1, make([]byte, 8)); err == nil {
		t.Error("short previous key accepted")
	}
}

func TestAPITokenShape(t *testing.T) {
	tok := NewAPIToken()
	if !strings.HasPrefix(tok, "kwft_") || !LooksLikeAPIToken(tok) || NewAPIToken() == tok {
		t.Fatalf("token %q", tok)
	}
	if h := APITokenHint(tok); !strings.HasPrefix(tok, strings.TrimSuffix(h, "…")) || len(h) > 16 {
		t.Errorf("hint %q", h)
	}
	for _, bad := range []string{"", "kwft_", "kwft_short", NewToken(), "kwft_" + NewToken() + "x", "Kwft_" + NewToken(), "kwft_" + strings.Repeat("!", 43)} {
		if LooksLikeAPIToken(bad) {
			t.Errorf("%q looks like a token", bad)
		}
	}
}

func TestParseDataKey(t *testing.T) {
	hexKey := strings.Repeat("ab", 32)
	if k, err := ParseDataKey(hexKey); err != nil || len(k) != 32 || k[0] != 0xab {
		t.Errorf("hex: %v %v", k, err)
	}
	if k, err := ParseDataKey(" " + NewDataKey() + "\n"); err != nil || len(k) != 32 {
		t.Errorf("base64 with whitespace: %v", err)
	}
	for _, bad := range []string{"", "short", "c2hvcnQ=", strings.Repeat("zz", 32)} {
		if _, err := ParseDataKey(bad); err == nil {
			t.Errorf("ParseDataKey(%q) accepted", bad)
		}
	}
	if _, err := NewSealer(make([]byte, 16)); err == nil {
		t.Error("16-byte key accepted")
	}
}
