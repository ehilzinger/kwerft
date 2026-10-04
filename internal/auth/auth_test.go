package auth

import (
	"strings"
	"testing"
)

func TestPasswordRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Errorf("unexpected hash format %q", h)
	}
	if ok, err := VerifyPassword(h, "correct horse battery staple"); !ok || err != nil {
		t.Errorf("right password rejected: %v", err)
	}
	if ok, _ := VerifyPassword(h, "correct horse battery stapl"); ok {
		t.Error("wrong password accepted")
	}
	h2, _ := HashPassword("correct horse battery staple")
	if h == h2 {
		t.Error("hashes of the same password must differ (salt)")
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, h := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$AA$AA", "$argon2id$v=19$m=x$AA$AA"} {
		if ok, err := VerifyPassword(h, "whatever"); ok || err == nil {
			t.Errorf("VerifyPassword(%q) = %v, %v; want false with error", h, ok, err)
		}
	}
}

func TestCheckPassword(t *testing.T) {
	if CheckPassword("short") == nil {
		t.Error("short password accepted")
	}
	if CheckPassword("twelve chars") != nil {
		t.Error("12-character password rejected")
	}
	if CheckPassword(strings.Repeat("x", MaxPasswordLen+1)) == nil {
		t.Error("over-long password accepted")
	}
}

func TestTokenMatches(t *testing.T) {
	tok := NewToken()
	if !TokenMatches(tok, HashToken(tok)) || !TokenMatches("  "+tok+"\n", strings.ToUpper(HashToken(tok))) {
		t.Error("token should match its hash, ignoring surrounding whitespace and hex case")
	}
	if TokenMatches(tok+"x", HashToken(tok)) {
		t.Error("different token matched")
	}
}
