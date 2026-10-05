package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// volumeWaitRequeue is how often an App or Task whose pods wait for a disk
// looks again: the kubelet's mount failures are only events, which no
// watch here brings in.
const volumeWaitRequeue = 30 * time.Second

// maxVolumeWait caps the kubelet's message in a status message.
const maxVolumeWait = 400

// volumeWait explains why one of the pods waits in ContainerCreating for a
// disk, from the kubelet's latest FailedMount or the attach/detach
// controller's FailedAttachVolume event for it; the pod's own status only
// says ContainerCreating. "" when none of them has such an event.
//
// A Hetzner Cloud Volume mounted read-only on a node cannot be mounted
// read-write there as well: its CSI driver mounts the device itself for
// every pod (no staging mount to bind from), so the first read-only mount
// makes the filesystem read-only and every read-write mount after it fails
// with EBUSY. Kwerft renders read-only shared Volumes as read-only container
// mounts over a read-write device mount since, but pods started before keep
// theirs until they stop; the message names them.
func volumeWait(ctx context.Context, r client.Reader, pods []corev1.Pod) string {
	if r == nil {
		return ""
	}
	pods = slices.Clone(pods)
	slices.SortFunc(pods, func(a, b corev1.Pod) int { return strings.Compare(a.Name, b.Name) })
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodPending || !p.DeletionTimestamp.IsZero() || p.Spec.NodeName == "" {
			continue
		}
		var events corev1.EventList
		if err := r.List(ctx, &events, client.InNamespace(p.Namespace),
			client.MatchingFields{"involvedObject.uid": string(p.UID)}); err != nil {
			continue // only the message is missing
		}
		e := volumeEvent(events.Items)
		if e == nil {
			continue
		}
		if strings.Contains(strings.ToLower(e.Message), "busy") {
			if msg := readOnlyHolders(ctx, r, p); msg != "" {
				return msg
			}
		}
		return "waiting for a volume: " + shorten(e.Message, maxVolumeWait)
	}
	return ""
}

// volumeEvent picks the event that says most about a pod's disk: the latest
// FailedMount or FailedAttachVolume, preferring the driver's own error over
// the kubelet's "Unable to attach or mount volumes: … timed out" summary.
func volumeEvent(events []corev1.Event) *corev1.Event {
	var best *corev1.Event
	rank := func(e *corev1.Event) int {
		if strings.HasPrefix(e.Message, "Unable to attach or mount volumes") {
			return 0
		}
		return 1
	}
	for i := range events {
		e := &events[i]
		if e.Reason != "FailedMount" && e.Reason != "FailedAttachVolume" {
			continue
		}
		if best == nil || rank(e) > rank(best) ||
			(rank(e) == rank(best) && eventTime(e).After(eventTime(best))) {
			best = e
		}
	}
	return best
}

func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	}
	return e.CreationTimestamp.Time
}

// readOnlyHolders names the pods on p's node that mount one of p's claims
// read-only at the volume level, which holds the device read-only there
// (see volumeWait); "" when there are none.
func readOnlyHolders(ctx context.Context, r client.Reader, p *corev1.Pod) string {
	claims := map[string]bool{}
	for _, v := range p.Spec.Volumes {
		if c := v.PersistentVolumeClaim; c != nil {
			claims[c.ClaimName] = true
		}
	}
	if len(claims) == 0 {
		return ""
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(p.Namespace)); err != nil {
		return ""
	}
	var volumes, holders []string
	for _, o := range pods.Items {
		if o.UID == p.UID || o.Spec.NodeName != p.Spec.NodeName ||
			o.Status.Phase == corev1.PodSucceeded || o.Status.Phase == corev1.PodFailed {
			continue
		}
		held := false
		for _, v := range o.Spec.Volumes {
			if c := v.PersistentVolumeClaim; c != nil && c.ReadOnly && claims[c.ClaimName] {
				held = true
				if !slices.Contains(volumes, c.ClaimName) {
					volumes = append(volumes, c.ClaimName)
				}
			}
		}
		if held {
			if h := podOwner(&o); !slices.Contains(holders, h) {
				holders = append(holders, h)
			}
		}
	}
	if len(holders) == 0 {
		return ""
	}
	slices.Sort(volumes)
	slices.Sort(holders)
	return fmt.Sprintf("Volume %s is mounted read-only on node %s by %s, started by an earlier Kwerft; "+
		"it cannot be mounted read-write there until they stop. "+
		"Stop and start those Apps (a restart waits the same way), or let those Tasks finish",
		strings.Join(volumes, ", "), p.Spec.NodeName, strings.Join(holders, ", "))
}

// podOwner names what a Kwerft pod belongs to: App or Task, else the pod.
func podOwner(p *corev1.Pod) string {
	switch {
	case p.Labels[LabelApp] != "":
		return "App " + p.Labels[LabelApp]
	case p.Labels[LabelTask] != "":
		return "Task " + p.Labels[LabelTask]
	}
	return "pod " + p.Name
}

// shorten folds whitespace (the kubelet's mount errors span lines) and cuts
// s to at most n runes.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
