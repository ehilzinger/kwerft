// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Package backups holds what the console's backup code shares and that
// needs no Kubernetes: the recovery key's format, a small S3 client (SigV4)
// for the connection check and the etcd snapshot uploads (etcd.go), and a
// reader for the contents of a Velero backup (the tarball of its objects).
// docs/phase6.md › Backups.
package backups

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"strings"
)

// The recovery key is the Kopia repository password Velero encrypts volume
// data with. It is 32 random bytes, shown as 52 base32 characters in 13
// groups of 4 joined by dashes ("ABCD-EFGH-…"). The repository password is
// its 52 characters, upper case, without separators (RepositoryPassword);
// install.sh --restore normalizes a key file the same way before it writes
// velero-repo-credentials.

const (
	recoveryKeyBytes  = 32
	recoveryKeyChars  = 52
	recoveryKeyGroups = 4
)

var keyEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewRecoveryKey makes a recovery key.
func NewRecoveryKey() string {
	b := make([]byte, recoveryKeyBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	return group(keyEncoding.EncodeToString(b))
}

// ErrRecoveryKey: the text is not a recovery key.
var ErrRecoveryKey = errors.New("not a recovery key: 52 letters and digits (A–Z, 2–7), in groups of 4")

// NormalizeRecoveryKey reads a recovery key as someone may type or paste
// it (any case, with or without dashes and spaces) and returns its written
// form.
func NormalizeRecoveryKey(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r == '-' || r == ' ' || r == '\t' || r == '\n' || r == '\r':
		default:
			b.WriteRune(r)
		}
	}
	raw := b.String()
	if len(raw) != recoveryKeyChars {
		return "", ErrRecoveryKey
	}
	dec, err := keyEncoding.DecodeString(raw)
	if err != nil || len(dec) != recoveryKeyBytes {
		return "", ErrRecoveryKey
	}
	return group(raw), nil
}

// RepositoryPassword is the Kopia repository password of a recovery key:
// its 52 characters without separators. ErrRecoveryKey when s is no key.
func RepositoryPassword(s string) (string, error) {
	k, err := NormalizeRecoveryKey(s)
	if err != nil {
		return "", err
	}
	return strings.ReplaceAll(k, "-", ""), nil
}

func group(raw string) string {
	parts := make([]string, 0, len(raw)/recoveryKeyGroups+1)
	for i := 0; i < len(raw); i += recoveryKeyGroups {
		parts = append(parts, raw[i:min(i+recoveryKeyGroups, len(raw))])
	}
	return strings.Join(parts, "-")
}

// RecoveryKeyFromFile reads a recovery key file as install.sh --restore
// does (recovery_key): the file Settings › Backups offers for download has
// the key on a line of its own between lines that say what it is for; a
// file of just the key may split it over lines. The first line that is a
// key wins, else the whole text.
func RecoveryKeyFromFile(text string) (string, error) {
	for line := range strings.Lines(text) {
		if k, err := NormalizeRecoveryKey(line); err == nil {
			return k, nil
		}
	}
	return NormalizeRecoveryKey(text)
}
