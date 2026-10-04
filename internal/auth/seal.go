package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// DataKeyLen is the size of the data key: AES-256.
const DataKeyLen = 32

// Sealed value formats. v1 (Kwerft ≤ 0.1) names no key; v2 names the key by
// its ID (KeyID), so a value can be opened while several keys are known and
// rotation can tell which values still need re-sealing.
const (
	sealPrefixV1 = "v1:"
	sealPrefixV2 = "v2:"
)

var ErrSealed = errors.New("cannot decrypt: wrong data key or damaged value")

// Sealer encrypts small secrets at rest (TOTP seeds) with AES-256-GCM. The
// context string is bound to the ciphertext as additional data, so a value
// copied into another row or column does not decrypt.
//
// A Sealer holds a key ring: the primary key seals, every key opens. Rotate
// makes a new key primary in place, so code holding the Sealer keeps working
// across a rotation.
type Sealer struct {
	mu      sync.RWMutex
	primary *sealKey
	keys    map[string]*sealKey // by ID, the primary included
	order   []*sealKey          // primary first: the order v1 values are tried in
}

type sealKey struct {
	id   string
	aead cipher.AEAD
}

// NewDataKey returns a fresh key in the text form ParseDataKey reads.
func NewDataKey() string {
	b := make([]byte, DataKeyLen)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// EncodeDataKey is the text form of a key (standard base64), as stored in
// the kwerft-data-key Secret.
func EncodeDataKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

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

// KeyID names a key without revealing it: the first 8 hex digits of the
// SHA-256 of "kwerft-data-key:" and the key. Shown in Settings and stored in
// v2 sealed values.
func KeyID(key []byte) string {
	sum := sha256.Sum256(append([]byte("kwerft-data-key:"), key...))
	return hex.EncodeToString(sum[:4])
}

func newSealKey(key []byte) (*sealKey, error) {
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
	return &sealKey{id: KeyID(key), aead: aead}, nil
}

// NewSealer returns a Sealer that seals with key and also opens values sealed
// with any of the previous keys (a rotation in progress).
func NewSealer(key []byte, previous ...[]byte) (*Sealer, error) {
	p, err := newSealKey(key)
	if err != nil {
		return nil, err
	}
	s := &Sealer{primary: p, keys: map[string]*sealKey{p.id: p}, order: []*sealKey{p}}
	for _, k := range previous {
		if err := s.add(k); err != nil {
			return nil, fmt.Errorf("previous data key: %w", err)
		}
	}
	return s, nil
}

func (s *Sealer) add(key []byte) error {
	k, err := newSealKey(key)
	if err != nil {
		return err
	}
	if _, ok := s.keys[k.id]; ok {
		return nil
	}
	s.keys[k.id] = k
	s.order = append(s.order, k)
	return nil
}

// Rotate makes key the primary key. The old keys stay in the ring, so
// values sealed with them still open until they are re-sealed (Reseal).
func (s *Sealer) Rotate(key []byte) error {
	k, err := newSealKey(key)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.keys[k.id]; ok {
		k = old
	}
	s.keys[k.id] = k
	order := []*sealKey{k}
	for _, o := range s.order {
		if o.id != k.id {
			order = append(order, o)
		}
	}
	s.primary, s.order = k, order
	return nil
}

// Forget drops every key but the primary, once nothing sealed with them is
// left.
func (s *Sealer) Forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = map[string]*sealKey{s.primary.id: s.primary}
	s.order = []*sealKey{s.primary}
}

// PrimaryID is the ID of the key that seals.
func (s *Sealer) PrimaryID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primary.id
}

// KeyIDs lists the IDs of all known keys, primary first.
func (s *Sealer) KeyIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.order))
	for i, k := range s.order {
		out[i] = k.id
	}
	return out
}

// Seal encrypts plaintext with the primary key and a random nonce.
func (s *Sealer) Seal(plaintext []byte, context string) string {
	s.mu.RLock()
	k := s.primary
	s.mu.RUnlock()
	nonce := make([]byte, k.aead.NonceSize())
	_, _ = rand.Read(nonce)
	out := k.aead.Seal(nonce, nonce, plaintext, []byte(context))
	return sealPrefixV2 + k.id + ":" + base64.RawStdEncoding.EncodeToString(out)
}

// Open reverses Seal; it fails with ErrSealed for an unknown key, a wrong
// key or context, or a damaged value.
func (s *Sealer) Open(sealed, context string) ([]byte, error) {
	pt, _, err := s.open(sealed, context)
	return pt, err
}

func (s *Sealer) open(sealed, context string) ([]byte, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var candidates []*sealKey
	var rest string
	if r, ok := strings.CutPrefix(sealed, sealPrefixV2); ok {
		id, body, ok := strings.Cut(r, ":")
		k := s.keys[id]
		if !ok || k == nil {
			return nil, "", ErrSealed
		}
		candidates, rest = []*sealKey{k}, body
	} else if r, ok := strings.CutPrefix(sealed, sealPrefixV1); ok {
		candidates, rest = s.order, r
	} else {
		return nil, "", ErrSealed
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return nil, "", ErrSealed
	}
	for _, k := range candidates {
		n := k.aead.NonceSize()
		if len(raw) < n {
			return nil, "", ErrSealed
		}
		if pt, err := k.aead.Open(nil, raw[:n], raw[n:], []byte(context)); err == nil {
			return pt, k.id, nil
		}
	}
	return nil, "", ErrSealed
}

// Current reports whether sealed is in the newest format and sealed with the
// primary key, i.e. needs no re-sealing.
func (s *Sealer) Current(sealed string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return strings.HasPrefix(sealed, sealPrefixV2+s.primary.id+":")
}

// Reseal opens a value and seals it again with the primary key. changed is
// false when it already was (then out is sealed unchanged).
func (s *Sealer) Reseal(sealed, context string) (out string, changed bool, err error) {
	if s.Current(sealed) {
		return sealed, false, nil
	}
	pt, _, err := s.open(sealed, context)
	if err != nil {
		return "", false, err
	}
	return s.Seal(pt, context), true, nil
}
