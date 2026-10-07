// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package backups

import (
	"crypto/hkdf"
	"crypto/md5" //nolint:gosec // S3's SSE-C protocol names MD5 for the key's checksum header
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

// Everything Velero writes to the bucket through its AWS plugin (the
// backups' object tarballs with every Secret, their logs, lists and volume
// info) is encrypted by the storage with a customer-provided key (SSE-C,
// AES-256). That key is derived from the recovery key, so the recovery key
// stays the only secret a restore needs (docs/phase6.md › As built (B3)):
//
//	HKDF-SHA256(secret = the repository password, i.e. the key's 52
//	            characters, upper case, without separators,
//	            salt   = "kwerft.dev/recovery-key",
//	            info   = "kwerft.dev/backups/sse-c/v1", 32 bytes)
//
// install.sh --restore derives the same 32 bytes (sse_customer_key); both
// are tested against the same vector. Volume data (Kopia) is encrypted with
// the repository password itself and goes without SSE-C.

const (
	sseSalt = "kwerft.dev/recovery-key"
	sseInfo = "kwerft.dev/backups/sse-c/v1"
	// SSEKeyBytes is the length of an SSE-C key (AES-256).
	SSEKeyBytes = 32
)

// SSECustomerKey derives the SSE-C key of a recovery key (any form
// NormalizeRecoveryKey reads). ErrRecoveryKey when s is no key.
func SSECustomerKey(s string) ([]byte, error) {
	pw, err := RepositoryPassword(s)
	if err != nil {
		return nil, err
	}
	return hkdf.Key(sha256.New, []byte(pw), []byte(sseSalt), sseInfo, SSEKeyBytes)
}

// SSE-C request headers.
const (
	HeaderSSECAlgorithm = "X-Amz-Server-Side-Encryption-Customer-Algorithm"
	HeaderSSECKey       = "X-Amz-Server-Side-Encryption-Customer-Key"
	HeaderSSECKeyMD5    = "X-Amz-Server-Side-Encryption-Customer-Key-Md5"
)

// SetSSEC sets the three SSE-C headers of a request for key: the algorithm
// (AES256), the key and its MD5, both base64.
func SetSSEC(h http.Header, key []byte) {
	sum := md5.Sum(key) //nolint:gosec // see the import
	h.Set(HeaderSSECAlgorithm, "AES256")
	h.Set(HeaderSSECKey, base64.StdEncoding.EncodeToString(key))
	h.Set(HeaderSSECKeyMD5, base64.StdEncoding.EncodeToString(sum[:]))
}

// PresignedWithSSEC reports whether a presigned URL (a DownloadRequest's)
// was signed for SSE-C: its X-Amz-SignedHeaders name the customer key, so
// the GET must send the three headers (the key never goes into the URL).
func PresignedWithSSEC(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	for _, h := range strings.Split(u.Query().Get("X-Amz-SignedHeaders"), ";") {
		if strings.EqualFold(h, HeaderSSECKey) {
			return true
		}
	}
	return false
}
