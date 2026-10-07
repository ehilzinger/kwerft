// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package auth holds password hashing, session tokens and the request
// checks the HTTP API relies on.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: OWASP's 19 MiB / 2 iterations profile. Memory stays
// modest so a burst of logins cannot push the pod past its limit.
const (
	argonMemory  = 19 * 1024
	argonTime    = 2
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16

	MinPasswordLen = 12
	MaxPasswordLen = 256
)

var ErrWeakPassword = fmt.Errorf("use at least %d characters", MinPasswordLen)

// ValidEmail accepts a plain address (you@example.com) whose domain has a dot:
// what sign-in, invitations and the owner account use.
func ValidEmail(s string) bool {
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Address == s && len(s) <= 254 && strings.Contains(s[strings.LastIndex(s, "@"):], ".")
}

// CheckPassword enforces length only: long passphrases beat composition rules.
func CheckPassword(pw string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinPasswordLen {
		return ErrWeakPassword
	}
	if n > MaxPasswordLen {
		return fmt.Errorf("use at most %d characters", MaxPasswordLen)
	}
	return nil
}

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash from HashPassword in constant time.
// Parameters are read from the hash, so stronger settings can roll out later.
func VerifyPassword(hash, pw string) (bool, error) {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, errors.New("unsupported password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("unsupported argon2 version")
	}
	var mem, iters uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &iters, &threads); err != nil {
		return false, fmt.Errorf("bad argon2 parameters: %w", err)
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, iters, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is verified against when a login names an unknown user, so the
// response takes as long as for a real one.
var dummyHash = func() string {
	h, err := HashPassword("not-a-real-password-just-for-timing")
	if err != nil {
		panic(err)
	}
	return h
}()

// DummyVerify burns the same time as a real VerifyPassword.
func DummyVerify(pw string) { _, _ = VerifyPassword(dummyHash, pw) }
