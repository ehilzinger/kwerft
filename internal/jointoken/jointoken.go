// Package jointoken issues and checks the short-lived tokens that let a
// server join a cluster (docs/phase5.md › Join material).
//
// A join token never contains a Kubernetes credential. The installer trades
// it at the console (GET /api/v1/join) for the k3s join material:
//
//   - workers get a k3s bootstrap token (a kubeadm-style Secret
//     bootstrap-token-<id> in kube-system, the same as `k3s token create`)
//     that expires within the hour, so a leaked join answer does not hand
//     out the cluster's server token;
//   - control-plane servers get the k3s server token: k3s encrypts its
//     bootstrap data (CA keys, etcd peers) with it, and a joining server
//     must be able to decrypt them. That is why only owners and admins may
//     ask for a control-plane join command.
//
// Join tokens are signed (HMAC-SHA256 with a key derived from the console's
// data key), not stored: they carry the cluster, the role, an expiry and,
// for servers Kwerft creates, the node name they are bound to. A token
// bound to a node is refused once a node of that name exists, so the
// cloud-init token of a joined server is spent. Rotating the data key
// invalidates every outstanding join token.
package jointoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Prefix starts every join token (secret scanners can match it).
const Prefix = "kwft_join_"

// Roles a token can join as.
const (
	RoleWorker       = "worker"
	RoleControlPlane = "control-plane"
)

// MaxTTL bounds how long a join token lives.
const MaxTTL = 24 * time.Hour

var (
	// ErrInvalid: not a join token, or not signed by this console.
	ErrInvalid = errors.New("invalid join token")
	// ErrExpired: the token's time is up.
	ErrExpired = errors.New("join token expired")
)

// Claims is what a token allows.
type Claims struct {
	Cluster string `json:"c"`
	Role    string `json:"r"`
	// Node, when set, is the one server name that may use the token.
	Node string `json:"n,omitempty"`
	// Expires is a Unix time.
	Expires int64 `json:"e"`
	// ID tells tokens apart in the audit log.
	ID string `json:"i"`
}

// ExpiresAt is Expires as a time.
func (c Claims) ExpiresAt() time.Time { return time.Unix(c.Expires, 0) }

// Signer issues and verifies join tokens with one key.
type Signer struct {
	key []byte
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NewSigner derives the signing key from the console's data key; nil or
// empty keys give nil (join tokens off).
func NewSigner(dataKey []byte) *Signer {
	if len(dataKey) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, dataKey)
	m.Write([]byte("kwerft join tokens v1"))
	return &Signer{key: m.Sum(nil)}
}

func (s *Signer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Issue signs a token valid for ttl (at most MaxTTL). It sets ID and Expires.
func (s *Signer) Issue(c Claims, ttl time.Duration) (string, Claims, error) {
	if s == nil {
		return "", c, errors.New("join tokens need the console's data key")
	}
	if c.Cluster == "" || (c.Role != RoleWorker && c.Role != RoleControlPlane) {
		return "", c, errors.New("a join token needs a cluster and a role")
	}
	if ttl <= 0 || ttl > MaxTTL {
		return "", c, errors.New("a join token lives between a minute and 24 hours")
	}
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		return "", c, err
	}
	c.ID = base64.RawURLEncoding.EncodeToString(id)
	c.Expires = s.now().Add(ttl).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", c, err
	}
	p := base64.RawURLEncoding.EncodeToString(payload)
	return Prefix + p + "." + base64.RawURLEncoding.EncodeToString(s.sign(p)), c, nil
}

func (s *Signer) sign(payload string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Verify checks the signature and expiry and returns the claims.
func (s *Signer) Verify(token string) (Claims, error) {
	if s == nil || !strings.HasPrefix(token, Prefix) || len(token) > 1024 {
		return Claims{}, ErrInvalid
	}
	p, sig, ok := strings.Cut(strings.TrimPrefix(token, Prefix), ".")
	if !ok {
		return Claims{}, ErrInvalid
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, s.sign(p)) {
		return Claims{}, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, ErrInvalid
	}
	var c Claims
	if err := json.Unmarshal(raw, &c); err != nil || c.Cluster == "" {
		return Claims{}, ErrInvalid
	}
	if !s.now().Before(c.ExpiresAt()) {
		return c, ErrExpired
	}
	return c, nil
}
