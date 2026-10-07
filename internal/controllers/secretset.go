// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Secret sets (docs/plan.md › Secrets): what the SecretSet reconciler, the
// App reconciler and the console API share.

const (
	// LabelSecretSet marks the Secret a SecretSet manages; its value is the
	// set's name (which is also the Secret's). A Secret without it is never
	// adopted.
	LabelSecretSet = "kwerft.dev/secret-set"

	// KeyAnnotationPrefix + KeyAnnotationName(key) on a set's Secret records
	// when and by whom a key was last written, as a KeyRecord (JSON). The
	// console API writes it with the value; the reconciler for generated and
	// derived keys. Values are never in it.
	KeyAnnotationPrefix = "key.secret-set.kwerft.dev/"

	// SecretSetsRole (and its RoleBinding) in a project namespace grants
	// patch on exactly the managed Secrets there: to owners, admins and the
	// project's developers. SecretSetsReadRole grants get on them: owners and
	// admins (reveal). Both are kept by the SecretSet reconciler.
	SecretSetsRole     = "kwerft:secret-sets"
	SecretSetsReadRole = "kwerft:secret-sets-read"

	// AnnotationSecretsHash on an App's pod template hashes the values of the
	// Secret keys its env references, so a changed value rolls the App
	// without a new revision.
	AnnotationSecretsHash = "kwerft.dev/secrets-hash"

	// Where a key's value came from (SecretKeyStatus.Source).
	SourceSet       = "Set"
	SourceGenerated = "Generated"
	SourceDerived   = "Derived"

	// AppEnvSetSuffix: an App's own set is <app>-env, owned by the App.
	AppEnvSetSuffix = "-env"
)

// AppEnvSet is the name of an App's own SecretSet.
func AppEnvSet(app string) string { return app + AppEnvSetSuffix }

// SecretKeyRE is what a key of a Secret may be called (as the CRD says for
// derived keys).
var SecretKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]{1,253}$`)

// KeyRecord is the per-key annotation: who wrote a key when, and how.
type KeyRecord struct {
	At     time.Time `json:"at"`
	By     string    `json:"by,omitempty"`
	Source string    `json:"source"`
}

// Encode is the annotation value.
func (k KeyRecord) Encode() string {
	b, _ := json.Marshal(KeyRecord{At: k.At.UTC().Truncate(time.Second), By: k.By, Source: k.Source})
	return string(b)
}

// KeyAnnotation is the annotation holding key's KeyRecord. An annotation
// name is at most 63 characters and starts and ends with a letter or digit;
// keys that are not such a name are hashed.
func KeyAnnotation(key string) string {
	if len(validation.IsQualifiedName(KeyAnnotationPrefix+key)) == 0 && !strings.Contains(key, "/") {
		return KeyAnnotationPrefix + key
	}
	sum := sha256.Sum256([]byte(key))
	return KeyAnnotationPrefix + "x." + hex.EncodeToString(sum[:20])
}

// keyRecord reads key's record from a Secret's annotations.
func keyRecord(annotations map[string]string, key string) (KeyRecord, bool) {
	raw, ok := annotations[KeyAnnotation(key)]
	if !ok {
		return KeyRecord{}, false
	}
	var rec KeyRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return KeyRecord{}, false
	}
	return rec, true
}

// GenerateSecretValue is 32 random bytes, base64url without padding (43
// characters, safe in URLs such as a DATABASE_URL).
func GenerateSecretValue() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// templateRef matches ${KEY} in a derived key's template.
var templateRef = regexp.MustCompile(`\$\{([-._a-zA-Z0-9]+)\}`)

// TemplateKeys lists the keys a derived template references, in order.
func TemplateKeys(template string) []string {
	var out []string
	for _, m := range templateRef.FindAllStringSubmatch(template, -1) {
		out = append(out, m[1])
	}
	return out
}

// renderTemplate fills ${KEY} from values; missing lists keys it lacks.
func renderTemplate(template string, values map[string][]byte) (string, []string) {
	var missing []string
	out := templateRef.ReplaceAllStringFunc(template, func(m string) string {
		key := m[2 : len(m)-1]
		v, ok := values[key]
		if !ok {
			missing = append(missing, key)
			return m
		}
		return string(v)
	})
	return out, missing
}

// envSecretRefs are the non-optional and optional Secret references of env.
func envSecretRefs(env []corev1.EnvVar) []*corev1.SecretKeySelector {
	var out []*corev1.SecretKeySelector
	for _, e := range env {
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
			out = append(out, e.ValueFrom.SecretKeyRef)
		}
	}
	return out
}

// secretRef is one env reference to a Secret key the App needs and lacks:
// key "" when the whole Secret is missing.
type secretRef struct{ secret, key string }

// secretRefs reads the Secrets env references (uncached, through r) and
// returns a hash of the referenced values, in a stable order, and the
// references that cannot be satisfied (optional ones never count). A nil
// reader or no references: "" and nothing missing.
//
// The hash is an HMAC under key (the Secret kwerft-secrets-hash-key): the
// pod template that carries it is readable by every role, viewers included,
// and a plain hash would let them confirm a guessed low-entropy value
// offline.
func secretRefs(ctx context.Context, r client.Reader, key []byte, namespace string, env []corev1.EnvVar) (string, []secretRef, error) {
	refs := envSecretRefs(env)
	if r == nil || len(refs) == 0 {
		return "", nil, nil
	}
	if len(key) == 0 {
		return "", nil, errors.New("no secrets hash key")
	}
	slices.SortFunc(refs, func(a, b *corev1.SecretKeySelector) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Key, b.Key))
	})
	secrets := map[string]*corev1.Secret{}
	var missing []secretRef
	h := hmac.New(sha256.New, key)
	for _, ref := range refs {
		sec, seen := secrets[ref.Name]
		if !seen {
			var s corev1.Secret
			switch err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, &s); {
			case apierrors.IsNotFound(err):
			case err != nil:
				return "", nil, err
			default:
				sec = &s
			}
			secrets[ref.Name] = sec
		}
		optional := ref.Optional != nil && *ref.Optional
		var value []byte
		var ok bool
		if sec != nil {
			value, ok = sec.Data[ref.Key]
		}
		switch {
		case ok, optional:
		case sec == nil:
			if !slices.Contains(missing, secretRef{secret: ref.Name}) {
				missing = append(missing, secretRef{secret: ref.Name})
			}
		default:
			missing = append(missing, secretRef{secret: ref.Name, key: ref.Key})
		}
		// Length-prefixed, so no two different inputs hash alike.
		fmt.Fprintf(h, "%d:%s%d:%s%t%d:", len(ref.Name), ref.Name, len(ref.Key), ref.Key, ok, len(value))
		h.Write(value)
	}
	return hex.EncodeToString(h.Sum(nil))[:32], missing, nil
}

// secretRefsMissing words secretRefs' missing references for the App's
// status.
func secretRefsMissing(missing []secretRef) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		if m.key == "" {
			parts = append(parts, fmt.Sprintf("Secret %q does not exist", m.secret))
		} else {
			parts = append(parts, fmt.Sprintf("%s has no key %s", m.secret, m.key))
		}
	}
	return "Waiting for secret values: " + strings.Join(parts, "; ") + ". Set them on the Secrets page."
}
