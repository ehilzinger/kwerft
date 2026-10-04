package auth

import (
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
	if !strings.HasPrefix(sealed, "v1:") || strings.Contains(sealed, "totp seed") {
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
	for _, bad := range []string{"", "v1:", "v1:!!!", "v2:" + sealed[3:], sealed[:len(sealed)-2]} {
		if _, err := s.Open(bad, "totp:user1"); !errors.Is(err, ErrSealed) {
			t.Errorf("Open(%q): %v, want ErrSealed", bad, err)
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
