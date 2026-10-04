// Package setup holds where the one-time setup token comes from. The
// installer writes the token to /etc/kwerft/setup-token on the server and only
// its sha256 hash into the cluster, so Kwerft never sees the token at rest.
package setup

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ehilzinger/kwerft/internal/auth"
)

// ErrNoToken means no setup token is active (never created, or already used).
var ErrNoToken = errors.New("no setup token")

// SecretName is the Secret the installer creates in Kwerft's namespace.
const SecretName = "kwerft-setup-token"

// TokenSource yields the active token's hash and consumes it once setup is done.
type TokenSource interface {
	Hash(ctx context.Context) (hash string, expires time.Time, err error)
	Consume(ctx context.Context) error
}

// SecretTokenSource reads the installer's Secret. Reads bypass the cache so a
// token re-created by the installer is seen immediately.
type SecretTokenSource struct {
	Reader    client.Reader
	Writer    client.Writer
	Namespace string
}

func (s *SecretTokenSource) Hash(ctx context.Context) (string, time.Time, error) {
	var sec corev1.Secret
	if err := s.Reader.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: SecretName}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return "", time.Time{}, ErrNoToken
		}
		return "", time.Time{}, err
	}
	hash := strings.TrimSpace(string(sec.Data["sha256"]))
	if hash == "" {
		return "", time.Time{}, ErrNoToken
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(string(sec.Data["expires"])))
	if err != nil {
		return "", time.Time{}, errors.New("setup token secret has no valid expiry")
	}
	return hash, expires, nil
}

// Consume deletes the Secret: the token works exactly once.
func (s *SecretTokenSource) Consume(ctx context.Context) error {
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: SecretName}}
	return client.IgnoreNotFound(s.Writer.Delete(ctx, sec))
}

// StaticTokenSource serves one in-memory token, for local development and
// tests (`kwerft --dev` prints it at start).
type StaticTokenSource struct {
	mu       sync.Mutex
	hash     string
	expires  time.Time
	consumed bool
}

func NewStaticTokenSource(token string, ttl time.Duration) *StaticTokenSource {
	return &StaticTokenSource{hash: auth.HashToken(token), expires: time.Now().Add(ttl)}
}

func (s *StaticTokenSource) Hash(context.Context) (string, time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.consumed {
		return "", time.Time{}, ErrNoToken
	}
	return s.hash, s.expires, nil
}

func (s *StaticTokenSource) Consume(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consumed = true
	return nil
}
