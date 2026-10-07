// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
)

// Debug shells: images built FROM scratch or distroless have no shell to exec
// into. Kubernetes can add an ephemeral container to a running pod that shares
// the target container's process namespace and the pod's network and volumes;
// the console adds a small toolbox there and opens the same recorded shell in
// it. The request goes to Kubernetes as the user (pods/ephemeralcontainers),
// so RBAC and the project's Pod Security level decide, as for any shell.

// DefaultDebugImage is the toolbox started for debug shells.
const DefaultDebugImage = "busybox:1.37"

const debugPrefix = "kwerft-debug-"

// errNoShell is how container runtimes report a missing shell binary.
func isNoShell(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "executable file not found") ||
		(strings.Contains(msg, "exec:") && strings.Contains(msg, "no such file or directory"))
}

// debugContainer is the toolbox container for a debug shell into target. It
// mirrors the target's user and its dropped capabilities, never adding any:
// that passes the project's Pod Security level (restricted apps run non-root
// and drop ALL, so the toolbox does too) and lets the toolbox read the app's
// files through /proc/<pid>/root, which the kernel only allows a process
// holding at least the target's capabilities. It sleeps a little longer than
// a shell may last and then exits; ephemeral containers cannot be removed,
// they go when the pod is replaced.
func debugContainer(pod *corev1.Pod, target, image string, lifetime time.Duration) corev1.EphemeralContainer {
	var user, group *int64
	var nonRoot *bool
	var drop []corev1.Capability
	if psc := pod.Spec.SecurityContext; psc != nil {
		user, group, nonRoot = psc.RunAsUser, psc.RunAsGroup, psc.RunAsNonRoot
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != target || c.SecurityContext == nil {
			continue
		}
		if sc := c.SecurityContext; sc.RunAsUser != nil {
			user = sc.RunAsUser
		}
		if sc := c.SecurityContext; sc.RunAsGroup != nil {
			group = sc.RunAsGroup
		}
		if sc := c.SecurityContext; sc.RunAsNonRoot != nil {
			nonRoot = sc.RunAsNonRoot
		}
		if sc := c.SecurityContext; sc.Capabilities != nil {
			drop = append(drop, sc.Capabilities.Drop...)
		}
	}
	if nonRoot != nil && *nonRoot && user == nil {
		user = ptr.To[int64](65534) // the image's own user is unknown here; nobody is safe
	}
	return corev1.EphemeralContainer{
		TargetContainerName: target,
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:    debugPrefix + randomSuffix(),
			Image:   image,
			Command: []string{"sleep", fmt.Sprint(int((lifetime + time.Minute).Seconds()))},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: user, RunAsGroup: group, RunAsNonRoot: nonRoot,
				AllowPrivilegeEscalation: ptr.To(false),
				Capabilities:             &corev1.Capabilities{Drop: drop},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		},
	}
}

func randomSuffix() string {
	const letters = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b)
}

// startDebug adds the toolbox to the pod and waits until it runs. It returns
// the container to exec into, or a message for the user.
func (s *shellSession) startDebug(ctx context.Context, b podBackend, pod *corev1.Pod) (string, string) {
	p := s.p
	if ok, err := b.allowed(ctx, s.project, "update", "ephemeralcontainers", s.pod); err != nil || !ok {
		if err == nil {
			p.audit(s.r, s.pr.user.Email, "pod.debug.denied", s.target(), "forbidden by Kubernetes RBAC")
			return "", "Your role does not allow debug shells."
		}
		return "", "Kubernetes did not answer: " + err.Error()
	}
	image := p.cfg.DebugImage
	if image == "" {
		image = DefaultDebugImage
	}
	ec := debugContainer(pod, s.container, image, p.shell.maxDuration)
	_ = s.ws.json(map[string]any{"type": "status", "message": fmt.Sprintf("Starting a debug container (%s) next to %s…", image, s.container)})
	if err := b.addDebugContainer(ctx, s.project, s.pod, ec); err != nil {
		switch {
		case apierrors.IsForbidden(err) && strings.Contains(err.Error(), "PodSecurity"):
			return "", "The project's security level does not allow this debug container: " + apiMessage(err)
		case apierrors.IsForbidden(err):
			return "", "Your role does not allow debug shells."
		default:
			return "", "The debug container could not be added: " + apiMessage(err)
		}
	}

	deadline := time.Now().Add(p.shell.debugStart)
	for {
		cur, err := b.getPod(ctx, s.project, s.pod)
		if err != nil {
			return "", kubeMessage(err, "The pod went away while the debug container was starting.")
		}
		for _, st := range cur.Status.EphemeralContainerStatuses {
			if st.Name != ec.Name {
				continue
			}
			switch {
			case st.State.Running != nil:
				return ec.Name, ""
			case st.State.Terminated != nil:
				return "", fmt.Sprintf("The debug container stopped right away (%s).", st.State.Terminated.Reason)
			case st.State.Waiting != nil && (st.State.Waiting.Reason == "ErrImagePull" || st.State.Waiting.Reason == "ImagePullBackOff" ||
				st.State.Waiting.Reason == "InvalidImageName" || strings.HasPrefix(st.State.Waiting.Reason, "CreateContainer")):
				return "", fmt.Sprintf("The debug container cannot start (%s): %s", st.State.Waiting.Reason, st.State.Waiting.Message)
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Sprintf("The debug container did not start within %s.", humanDuration(p.shell.debugStart))
		}
		select {
		case <-ctx.Done():
			return "", "Disconnected."
		case <-time.After(p.shell.debugPoll):
		}
	}
}
