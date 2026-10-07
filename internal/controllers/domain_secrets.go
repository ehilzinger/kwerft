// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1ac "sigs.k8s.io/gateway-api/applyconfiguration/apis/v1"
)

// Certificate secrets of hostnames the Gateway no longer serves. cert-manager
// deletes a listener's Certificate with the listener but keeps its secret.
// Kwerft removes such secrets after a grace period instead of at once: a
// hostname that comes back (an App renamed back, a Domain re-created) then
// reuses the still-valid certificate instead of spending one of Let's
// Encrypt's five certificates per hostname and week.

const (
	// AnnotationUnusedSince marks a listener secret no listener refers to.
	AnnotationUnusedSince = "kwerft.dev/unused-since"
	unusedSecretGrace     = 7 * 24 * time.Hour
	// certManagerCertName is set by cert-manager on the secrets it issues.
	certManagerCertName = "cert-manager.io/certificate-name"
)

// listenerSecretRE matches secrets named after Domain and console listeners
// (ListenerName, ConsoleListenerName): never the wildcard's or anyone else's.
var listenerSecretRE = regexp.MustCompile(`^[dc]-[a-z0-9-]*-[0-9a-f]{8}-tls$`)

// referencedSecrets are the certificate secrets the listeners use.
func referencedSecrets(listeners []*gwv1ac.ListenerApplyConfiguration) map[string]bool {
	out := map[string]bool{}
	for _, l := range listeners {
		if l.TLS == nil {
			continue
		}
		for _, ref := range l.TLS.CertificateRefs {
			if ref.Name != nil {
				out[string(*ref.Name)] = true
			}
		}
	}
	return out
}

// cleanupListenerSecrets marks unused listener secrets, unmarks used ones and
// deletes those unused for the grace period. It returns when it should look
// again (0: nothing pending).
func (r *DomainReconciler) cleanupListenerSecrets(ctx context.Context, used map[string]bool) (time.Duration, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var secrets corev1.SecretList
	if err := reader.List(ctx, &secrets, client.InNamespace(GatewayNamespace)); err != nil {
		return 0, err
	}
	now := r.now()
	var next time.Duration
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if sec.Type != corev1.SecretTypeTLS || sec.Annotations[certManagerCertName] == "" || !listenerSecretRE.MatchString(sec.Name) {
			continue
		}
		since, marked := sec.Annotations[AnnotationUnusedSince]
		if used[sec.Name] || r.certificateExists(ctx, sec.Name) {
			if marked {
				if err := r.annotateSecret(ctx, sec, nil); err != nil {
					return 0, err
				}
			}
			continue
		}
		at, err := time.Parse(time.RFC3339, since)
		if !marked || err != nil {
			stamp := now.UTC().Format(time.RFC3339)
			if err := r.annotateSecret(ctx, sec, &stamp); err != nil {
				return 0, err
			}
			at = now
		}
		left := unusedSecretGrace - now.Sub(at)
		if left <= 0 {
			if err := r.Delete(ctx, sec, client.Preconditions{UID: &sec.UID, ResourceVersion: &sec.ResourceVersion}); err != nil &&
				!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
				return 0, err
			}
			continue
		}
		if next == 0 || left < next {
			next = left
		}
	}
	return next, nil
}

func (r *DomainReconciler) certificateExists(ctx context.Context, name string) bool {
	cert := &unstructured.Unstructured{}
	cert.SetGroupVersionKind(certificateGVK)
	err := r.Get(ctx, client.ObjectKey{Namespace: GatewayNamespace, Name: name}, cert)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return false
	}
	return true // present, or unknown: keep the secret
}

// annotateSecret sets (or, with nil, removes) the unused-since mark.
func (r *DomainReconciler) annotateSecret(ctx context.Context, sec *corev1.Secret, since *string) error {
	orig := sec.DeepCopy()
	if sec.Annotations == nil {
		sec.Annotations = map[string]string{}
	}
	if since == nil {
		delete(sec.Annotations, AnnotationUnusedSince)
	} else {
		sec.Annotations[AnnotationUnusedSince] = *since
	}
	return client.IgnoreNotFound(r.Patch(ctx, sec, client.MergeFrom(orig)))
}
