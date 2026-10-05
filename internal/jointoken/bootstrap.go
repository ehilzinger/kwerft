package jointoken

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// k3s accepts kubeadm-style bootstrap tokens as agent tokens: a Secret
// bootstrap-token-<id> in kube-system of type bootstrap.kubernetes.io/token
// is what `k3s token create --ttl …` writes. The agent joins with
// "K10<CA hash>::<id>.<secret>" (the hash pins the cluster CA, as in the
// server token).

// BootstrapGroup is the group k3s puts its bootstrap-token agents in
// (k3s token create's default --groups).
const BootstrapGroup = "system:bootstrappers:k3s:default-node-token"

const tokenChars = "abcdefghijklmnopqrstuvwxyz0123456789"

// randomString draws n characters of tokenChars without modulo bias.
func randomString(n int) (string, error) {
	out := make([]byte, 0, n)
	buf := make([]byte, 2*n)
	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, b := range buf {
			if int(b) < 256-256%len(tokenChars) && len(out) < n {
				out = append(out, tokenChars[int(b)%len(tokenChars)])
			}
		}
	}
	return string(out), nil
}

// BootstrapToken is a new k3s bootstrap token: the Secret to create in the
// cluster and the agent token to hand to the joining node.
type BootstrapToken struct {
	Secret     *corev1.Secret
	AgentToken string
}

// NewBootstrapToken makes a bootstrap token that expires after ttl.
// serverToken is the cluster's k3s token (K10<hash>::server:<secret>); its
// CA hash is kept so the agent verifies the cluster it joins.
func NewBootstrapToken(serverToken, description string, ttl time.Duration, now time.Time) (BootstrapToken, error) {
	id, err := randomString(6)
	if err != nil {
		return BootstrapToken{}, err
	}
	secret, err := randomString(16)
	if err != nil {
		return BootstrapToken{}, err
	}
	agent := id + "." + secret
	if prefix, _, ok := strings.Cut(serverToken, "::"); ok && strings.HasPrefix(prefix, "K10") {
		agent = prefix + "::" + agent
	}
	return BootstrapToken{
		AgentToken: agent,
		Secret: &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: "bootstrap-token-" + id, Namespace: metav1.NamespaceSystem,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "kwerft"},
			},
			Type: corev1.SecretTypeBootstrapToken,
			StringData: map[string]string{
				"token-id":                       id,
				"token-secret":                   secret,
				"description":                    description,
				"expiration":                     now.Add(ttl).UTC().Format(time.RFC3339),
				"usage-bootstrap-authentication": "true",
				"usage-bootstrap-signing":        "true",
				"auth-extra-groups":              BootstrapGroup,
			},
		},
	}, nil
}

// ValidServerToken reports whether s looks like a k3s token.
func ValidServerToken(s string) error {
	if strings.TrimSpace(s) == "" || strings.ContainsAny(s, " \n\r\t") {
		return fmt.Errorf("the k3s token is empty or malformed")
	}
	return nil
}
