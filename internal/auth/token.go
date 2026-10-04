package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// NewToken returns a random URL-safe token with 256 bits of entropy.
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// HashToken is how tokens are stored and compared: never in plain text.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// TokenMatches compares a presented token with a stored hex sha256 hash in
// constant time.
func TokenMatches(token, wantHash string) bool {
	got := HashToken(token)
	return subtle.ConstantTimeCompare([]byte(got), []byte(strings.ToLower(strings.TrimSpace(wantHash)))) == 1
}

// APITokenPrefix starts every API token, so secret scanners (and people)
// recognise one in a leaked file or log.
const APITokenPrefix = "kwft_"

// NewAPIToken returns a new API token: the prefix and 256 random bits. Only
// its HashToken is stored.
func NewAPIToken() string { return APITokenPrefix + NewToken() }

// LooksLikeAPIToken reports whether s has the shape of an API token, so a
// stray value (a session cookie, a password) is never looked up as one.
func LooksLikeAPIToken(s string) bool {
	rest, ok := strings.CutPrefix(s, APITokenPrefix)
	if !ok || len(rest) != 43 {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(rest)
	return err == nil
}

// APITokenHint is what lists show of a token: the prefix and its first
// characters, enough to recognise it, useless to sign in with.
func APITokenHint(token string) string {
	if len(token) < len(APITokenPrefix)+6 {
		return APITokenPrefix + "…"
	}
	return token[:len(APITokenPrefix)+6] + "…"
}
