package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DataKeyLen is the size of the data key: AES-256.
const DataKeyLen = 32

// sealPrefix versions the format so the key can be rotated later.
const sealPrefix = "v1:"

var ErrSealed = errors.New("cannot decrypt: wrong data key or damaged value")

// Sealer encrypts small secrets at rest (TOTP seeds) with AES-256-GCM. The
// context string is bound to the ciphertext as additional data, so a value
// copied into another row or column does not decrypt.
type Sealer struct{ aead cipher.AEAD }

// NewDataKey returns a fresh key in the text form ParseDataKey reads.
func NewDataKey() string {
	b := make([]byte, DataKeyLen)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// ParseDataKey reads a 32-byte key given as base64 (standard or URL-safe,
// padded or not) or as 64 hex characters.
func ParseDataKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == 2*DataKeyLen {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != DataKeyLen {
				return nil, fmt.Errorf("data key is %d bytes, want %d", len(b), DataKeyLen)
			}
			return b, nil
		}
	}
	return nil, errors.New("data key is neither base64 nor hex")
}

func NewSealer(key []byte) (*Sealer, error) {
	if len(key) != DataKeyLen {
		return nil, fmt.Errorf("data key is %d bytes, want %d", len(key), DataKeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext with a random nonce.
func (s *Sealer) Seal(plaintext []byte, context string) string {
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := s.aead.Seal(nonce, nonce, plaintext, []byte(context))
	return sealPrefix + base64.RawStdEncoding.EncodeToString(out)
}

// Open reverses Seal; it fails with ErrSealed for a wrong key or context.
func (s *Sealer) Open(sealed, context string) ([]byte, error) {
	rest, ok := strings.CutPrefix(sealed, sealPrefix)
	if !ok {
		return nil, ErrSealed
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return nil, ErrSealed
	}
	n := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, raw[:n], raw[n:], []byte(context))
	if err != nil {
		return nil, ErrSealed
	}
	return pt, nil
}
