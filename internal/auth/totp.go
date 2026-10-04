package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; every authenticator app supports it
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// TOTP (RFC 6238) with the parameters every authenticator app understands:
// HMAC-SHA1, 6 digits, 30-second steps. One step either side is accepted to
// allow for clock drift; callers must refuse a step that was already used.
const (
	TOTPDigits = 6
	TOTPPeriod = 30 * time.Second
	totpSkew   = 1
	// 160 bits, the HMAC-SHA1 block output size RFC 4226 recommends.
	totpSecretLen = 20
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewTOTPSecret returns a fresh random shared secret.
func NewTOTPSecret() []byte {
	b := make([]byte, totpSecretLen)
	_, _ = rand.Read(b)
	return b
}

// TOTPSecretText is the secret as people type it into an authenticator app:
// base32 in groups of four.
func TOTPSecretText(secret []byte) string {
	s := b32.EncodeToString(secret)
	var parts []string
	for len(s) > 4 {
		parts = append(parts, s[:4])
		s = s[4:]
	}
	return strings.Join(append(parts, s), " ")
}

// TOTPURI is the otpauth:// URI that authenticator apps read from a QR code.
func TOTPURI(issuer, account string, secret []byte) string {
	q := url.Values{}
	q.Set("secret", b32.EncodeToString(secret))
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", fmt.Sprint(TOTPDigits))
	q.Set("period", fmt.Sprint(int(TOTPPeriod.Seconds())))
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) + "?" + q.Encode()
}

// TOTPStep is the RFC 6238 time step counter for t.
func TOTPStep(t time.Time) int64 { return t.Unix() / int64(TOTPPeriod.Seconds()) }

// TOTPCode returns the code for the step containing t.
func TOTPCode(secret []byte, t time.Time) string { return hotp(secret, TOTPStep(t), TOTPDigits) }

// hotp is RFC 4226 with dynamic truncation to the given number of digits.
func hotp(secret []byte, counter int64, digits int) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, secret)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff
	mod := uint32(1)
	for range digits {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, bin%mod)
}

// MatchTOTP checks code against the steps around now and returns the step it
// matched. It does not know which steps were used before: the caller stores
// the returned step and refuses it (and older ones) next time.
func MatchTOTP(secret []byte, code string, now time.Time) (step int64, ok bool) {
	code = NormalizeCode(code)
	if len(code) != TOTPDigits {
		return 0, false
	}
	cur := TOTPStep(now)
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		if subtle.ConstantTimeCompare([]byte(hotp(secret, s, TOTPDigits)), []byte(code)) == 1 {
			step, ok = s, true
		}
	}
	return step, ok
}

// NormalizeCode drops the spaces and dashes people type or paste into codes.
func NormalizeCode(code string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '-' || r == '\t' {
			return -1
		}
		return r
	}, strings.TrimSpace(code))
}

// LooksLikeTOTP reports whether s is shaped like an authenticator code (six
// digits), as opposed to a password, which is at least 12 characters.
func LooksLikeTOTP(s string) bool {
	s = NormalizeCode(s)
	if len(s) != TOTPDigits {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
