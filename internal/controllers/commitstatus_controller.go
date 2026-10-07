// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/git"
)

// Commit checks: the reporter follows Builds and tells the Git host about
// each phase once — pending while queued or running, then success, failure
// or cancelled — as a commit status (tokens) or a check run (GitHub Apps),
// linking to the build in the console. The phase last reported is recorded
// in an annotation on the Build, so restarts and resyncs do not report
// again. It is a controller of its own: a Git host that is down or refuses
// never holds up a build; failed reports are retried with the controller's
// exponential backoff, and given up when the host says they can never work
// (no permission, unknown repository).

const (
	// AnnotationReportedPhase is the Build phase last reported to the host.
	AnnotationReportedPhase = "kwerft.dev/reported-phase"
	// AnnotationCheckRun is the GitHub check run that reports the Build.
	AnnotationCheckRun = "kwerft.dev/check-run"

	// reportGiveUp: a phase this old is no longer worth reporting.
	reportGiveUp = 6 * time.Hour
)

// CommitStatusReconciler reports Builds to their Git host.
type CommitStatusReconciler struct {
	client.Client
	// APIReader reads the credentials Secrets; nil falls back to the client.
	APIReader client.Reader
	// Git talks to Git hosts; nil means a default Factory.
	Git *git.Factory
	// ConsoleDomain is the --console-domain flag (see GitConnectionReconciler).
	ConsoleDomain string
	Now           func() time.Time
}

func (r *CommitStatusReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// reportedPhase is the phase as reported: a Build the Build reconciler has
// not seen yet is pending too.
func reportedPhase(b *kwerftv1.Build) kwerftv1.BuildPhase {
	if b.Status.Phase == "" {
		return kwerftv1.BuildPending
	}
	return b.Status.Phase
}

// StatusContext names a Build's check on the commit, one per App.
func StatusContext(b *kwerftv1.Build) string {
	return "kwerft/" + b.Namespace + "/" + b.Spec.App
}

// BuildURL is the build's page in the console.
func BuildURL(host string, b *kwerftv1.Build) string {
	if host == "" {
		return ""
	}
	return "https://" + host + "/apps/" + url.PathEscape(b.Namespace) + "/" + url.PathEscape(b.Spec.App) + "?build=" + url.QueryEscape(b.Name)
}

func buildStatus(b *kwerftv1.Build) (git.State, string) {
	n := ""
	if b.Status.Number > 0 {
		n = fmt.Sprintf(" #%d", b.Status.Number)
	}
	switch reportedPhase(b) {
	case kwerftv1.BuildRunning:
		return git.StateRunning, "Build" + n + " is running"
	case kwerftv1.BuildSucceeded:
		msg := "Build" + n + " succeeded"
		if s, f := b.Status.StartTime, b.Status.CompletionTime; s != nil && f != nil {
			msg += " in " + f.Sub(s.Time).Round(time.Second).String()
		}
		if b.Spec.Deploy {
			msg += "; deploying"
		}
		return git.StateSuccess, msg
	case kwerftv1.BuildFailed:
		msg := "Build" + n + " failed"
		if b.Status.Message != "" {
			msg += ": " + b.Status.Message
		}
		return git.StateFailure, msg
	case kwerftv1.BuildCancelled:
		return git.StateCancelled, "Build" + n + " was cancelled"
	}
	msg := "Build" + n + " is queued"
	if b.Status.Message != "" {
		msg += ": " + b.Status.Message
	}
	return git.StatePending, msg
}

func (r *CommitStatusReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var b kwerftv1.Build
	if err := r.Get(ctx, req.NamespacedName, &b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	phase := reportedPhase(&b)
	if b.Spec.Source.Connection == "" || b.Annotations[AnnotationReportedPhase] == string(phase) || !b.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithValues("build", req.NamespacedName.String(), "phase", phase)

	var gc kwerftv1.GitConnection
	if err := r.Get(ctx, client.ObjectKey{Name: b.Spec.Source.Connection}, &gc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	conn := git.ConnectionFor(&gc, nil)
	if !conn.ReportsStatus() || !ProjectAllowed(&gc, b.Namespace) {
		return ctrl.Result{}, nil
	}
	repo, err := git.ParseRepository(b.Spec.Source.Repository)
	if err != nil || conn.Covers(repo) != nil {
		return ctrl.Result{}, nil
	}
	if done := b.Status.CompletionTime; done != nil && r.now().Sub(done.Time) > reportGiveUp {
		// e.g. the host was down for hours: a late check would only confuse.
		return ctrl.Result{}, r.markReported(ctx, &b, phase, b.Annotations[AnnotationCheckRun])
	}

	var sec corev1.Secret
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(gc.Name)}, &sec); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{RequeueAfter: time.Minute}, nil
		}
		return ctrl.Result{}, err
	}
	factory := r.Git
	if factory == nil {
		factory = &git.Factory{}
		r.Git = factory
	}
	provider, err := factory.For(git.ConnectionFor(&gc, sec.Data))
	if err != nil {
		logger.Info("cannot report the build", "err", err.Error())
		return ctrl.Result{}, nil
	}
	state, desc := buildStatus(&b)
	ref, err := provider.ReportStatus(ctx, repo, git.Status{
		SHA: b.Spec.Commit, State: state, Context: StatusContext(&b), Description: desc,
		TargetURL: BuildURL(ConsoleHost(ctx, r.Client, r.ConsoleDomain), &b), ExternalID: b.Namespace + "/" + b.Name,
		Ref: b.Annotations[AnnotationCheckRun],
	})
	if err != nil {
		if errors.Is(err, git.ErrUnauthorized) || errors.Is(err, git.ErrForbidden) || errors.Is(err, git.ErrNotFound) || errors.Is(err, git.ErrUnsupported) {
			// Retrying cannot help; the connection's page shows whether the
			// credentials work.
			logger.Info("the Git host refused the commit status; not retrying", "connection", gc.Name, "err", err.Error())
			return ctrl.Result{}, r.markReported(ctx, &b, phase, b.Annotations[AnnotationCheckRun])
		}
		logger.Info("reporting the commit status failed; retrying", "connection", gc.Name, "err", err.Error())
		return ctrl.Result{}, err // backoff
	}
	return ctrl.Result{}, r.markReported(ctx, &b, phase, ref)
}

func (r *CommitStatusReconciler) markReported(ctx context.Context, b *kwerftv1.Build, phase kwerftv1.BuildPhase, ref string) error {
	ann := map[string]any{AnnotationReportedPhase: string(phase)}
	if ref != "" {
		ann[AnnotationCheckRun] = ref
	}
	raw, err := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": ann}})
	if err != nil {
		return err
	}
	return client.IgnoreNotFound(r.Patch(ctx, b, client.RawPatch(types.MergePatchType, raw)))
}

func (r *CommitStatusReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("commitstatus").
		For(&kwerftv1.Build{}).
		Complete(r)
}
