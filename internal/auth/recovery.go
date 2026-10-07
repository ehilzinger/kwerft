// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"crypto/rand"
	"strings"
)

// RecoveryCodeCount is how many single-use recovery codes a user gets.
const RecoveryCodeCount = 10

// Crockford's base32 alphabet: no I, L, O or U, so codes survive being read
// aloud or copied by hand.
const recoveryAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// NewRecoveryCodes returns n codes like "k7rq-2m9x-dw4p-8hzt": 16 symbols,
// 80 bits each, so a plain sha256 of them is safe to store.
func NewRecoveryCodes(n int) []string {
	codes := make([]string, n)
	for i := range codes {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		var sb strings.Builder
		for j, v := range b {
			if j > 0 && j%4 == 0 {
				sb.WriteByte('-')
			}
			sb.WriteByte(recoveryAlphabet[v&31])
		}
		codes[i] = sb.String()
	}
	return codes
}

// HashRecoveryCode normalizes a typed code (case, dashes, spaces and
// look-alike letters) and hashes it for storage or lookup.
func HashRecoveryCode(code string) string {
	code = strings.ToLower(NormalizeCode(code))
	code = strings.NewReplacer("o", "0", "i", "1", "l", "1").Replace(code)
	return HashToken(code)
}
