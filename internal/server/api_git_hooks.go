// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/builds"
	"github.com/ehilzinger/kwerft/internal/clusters"
	"github.com/ehilzinger/kwerft/internal/controllers"
	"github.com/ehilzinger/kwerft/internal/git"
)

// Webhooks: POST /api/v1/hooks/git/{connection}, called by Git hosts.
//
// Authentication is the delivery's signature with the connection's webhook
// secret (HMAC-SHA256 of the body for GitHub and Gitea, the secret token for
// GitLab), checked in constant time before anything in the body is looked
// at. Unsigned or wrongly signed deliveries get 401 and change nothing. The
// endpoint has no session and no same-origin check (hosts are not
// browsers), a body limit and a per-address rate limit; rejected
// deliveries reach the audit log at most ten times per address in fifteen
// minutes.
//
// What is built:
//   - a push to a branch: every App using this connection whose repository
//     and branch match, in a project the connection serves. The Build
//     deploys when the App has autoDeploy (the default). Tag pushes and
//     branch deletions build nothing.
//   - a pull request opened, reopened or updated with new commits, whose
//     head branch is in the same repository: every such App whose branch is
//     the pull request's target, without deploying; the result appears as a
//     check on the pull request. Pull requests from forks are never built:
//     anyone can open one, and building it would run a stranger's Dockerfile
//     and build scripts on this server with the connection's credentials at
//     hand in the clone step, and fill the App's layer cache (which the next
//     production build reuses) with whatever they like. A branch in the same
//     repository can only be pushed by someone with write access, who could
//     push to the deployed branch anyway, so building it grants nothing new.
//
// Hosts redeliver (retries, "Redeliver" buttons). A webhook Build's name is
// derived from the App, trigger, pull request and commit, so a second
// delivery of the same event finds the Build already there and creates
// nothing. The Builds are created with the console's own identity — no
// user initiated them — and audited as git:<connection>.

type hookResult struct {
	Event    string   `json:"event"`
	Builds   []string `json:"builds"`
	Existing []string `json:"existing,omitempty"`
	Ignored  string   `json:"ignored,omitempty"`
}

func (g *gitAPI) hook(w http.ResponseWriter, r *http.Request) {
	if g.cfg.System == nil || g.cfg.SystemReader == nil {
		writeError(w, http.StatusServiceUnavailable, "This console is not connected to a Kubernetes cluster.")
		return
	}
	ip := clientIP(r)
	if !g.hookIP.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "Too many deliveries. Try again in a minute.")
		return
	}
	name := r.PathValue("connection")
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		writeError(w, http.StatusNotFound, "No such Git connection.")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, g.maxHookBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "The delivery is too large.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()

	var gc kwerftv1.GitConnection
	if err := g.cfg.System.Get(ctx, client.ObjectKey{Name: name}, &gc); err != nil {
		if apierrors.IsNotFound(err) {
			writeError(w, http.StatusNotFound, "No such Git connection.")
			return
		}
		g.internalError(w, r, err)
		return
	}
	var sec corev1.Secret
	secret := ""
	if err := g.cfg.SystemReader.Get(ctx, client.ObjectKey{Namespace: builds.Namespace, Name: builds.CredentialsSecret(name)}, &sec); err == nil {
		secret = string(sec.Data[builds.KeyWebhookSecret])
	} else if !apierrors.IsNotFound(err) {
		g.internalError(w, r, err)
		return
	}
	sec.Data = nil
	if err := git.VerifySignature(gc.Spec.Provider, r.Header, body, secret); err != nil {
		if g.hookFail.allow(ip) {
			g.audit(r, "anonymous", "git.webhook_rejected", name, "signature missing or wrong")
		}
		writeError(w, http.StatusUnauthorized, "The delivery's signature does not match the connection's webhook secret.")
		return
	}
	g.recordDelivery(ctx, name)

	ev, err := git.ParseEvent(gc.Spec.Provider, r.Header, body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "The delivery could not be read: "+err.Error())
		return
	}
	res := hookResult{Event: string(ev.Kind), Builds: []string{}}
	ignore := func(why string) {
		res.Ignored = why
		writeJSON(w, http.StatusOK, res)
	}
	var trigger string
	switch ev.Kind {
	case git.EventPing:
		ignore("ping: the webhook works")
		return
	case git.EventPush:
		switch {
		case ev.Deleted:
			ignore("branch deleted")
			return
		case ev.Tag:
			ignore("tag push")
			return
		}
		trigger = "push"
	case git.EventPullRequest:
		switch {
		case ev.Fork:
			g.cfg.Logger.Info("pull request from a fork not built", "connection", name, "pullRequest", ev.PullRequest)
			ignore("pull request from a fork: not built")
			return
		case !ev.Build:
			ignore("pull request " + ev.Action + ": nothing new to build")
			return
		}
		trigger = "pull-request"
	default:
		ignore("event not used")
		return
	}
	if !git.ValidSHA(ev.Commit.SHA) {
		writeError(w, http.StatusBadRequest, "The delivery names no commit.")
		return
	}

	apps, err := g.matchingApps(ctx, &gc, ev)
	if err != nil {
		g.internalError(w, r, err)
		return
	}
	if len(apps) == 0 {
		ignore("no app builds " + ev.Branch + " of this repository with this connection")
		return
	}
	actor := "git:" + name
	for i := range apps {
		app := &apps[i].app
		b, err := webhookBuild(app, ev, trigger)
		if err != nil {
			g.internalError(w, r, err)
			return
		}
		err = apps[i].conn.system.Create(ctx, b)
		switch {
		case apierrors.IsAlreadyExists(err):
			res.Existing = append(res.Existing, app.Namespace+"/"+b.Name)
			continue
		case err != nil:
			g.cfg.Logger.Error("creating a webhook build failed", "connection", name, "app", app.Namespace+"/"+app.Name, "err", err)
			writeError(w, http.StatusServiceUnavailable, "Could not create the build. The host will retry the delivery.")
			return
		}
		res.Builds = append(res.Builds, app.Namespace+"/"+b.Name)
		detail := fmt.Sprintf("%s of %s (%s) by %s", trigger, builds.Short(ev.Commit.SHA), ev.Branch, orNone(ev.Sender))
		if ev.PullRequest != 0 {
			detail = fmt.Sprintf("pull request #%d, %s (%s → %s) by %s", ev.PullRequest, builds.Short(ev.Commit.SHA), ev.HeadBranch, ev.Branch, orNone(ev.Sender))
		}
		if id := git.DeliveryID(r.Header); id != "" {
			detail += ", delivery " + id
		}
		g.audit(r, actor, "build.create", app.Namespace+"/"+b.Name, detail)
	}
	writeJSON(w, http.StatusAccepted, res)
}

// recordDelivery sets status.lastDelivery; a failure only costs the date.
func (g *gitAPI) recordDelivery(ctx context.Context, name string) {
	raw, _ := json.Marshal(map[string]any{"status": map[string]any{"lastDelivery": metav1.NewTime(g.now().UTC().Truncate(time.Second))}})
	gc := &kwerftv1.GitConnection{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := g.cfg.System.Status().Patch(ctx, gc, client.RawPatch(types.MergePatchType, raw)); err != nil {
		g.cfg.Logger.Warn("could not record the webhook delivery", "connection", name, "err", err)
	}
}

// hookApp is an App a delivery is for, with the cluster it lives in: its
// Build is created there.
type hookApp struct {
	app  kwerftv1.App
	conn *clusterConn
}

// matchingApps are the Apps a delivery is for, in every connected cluster
// (Git connections live in the management cluster and serve all of them,
// docs/phase5.md). A remote cluster that cannot be listed is skipped and
// logged; its apps build on the host's next delivery.
func (g *gitAPI) matchingApps(ctx context.Context, gc *kwerftv1.GitConnection, ev git.Event) ([]hookApp, error) {
	keys := map[string]bool{}
	for _, u := range ev.Repositories {
		if repo, err := git.ParseRepository(u); err == nil {
			keys[repo.Key()] = true
		}
	}
	var out []hookApp
	for _, st := range g.clusters.states() {
		conn := st.conn
		if conn == nil || conn.system == nil {
			if st.name != clusters.Local {
				g.cfg.Logger.Warn("git webhook: cluster skipped", "cluster", st.name, "connection", gc.Name)
			}
			continue
		}
		var list kwerftv1.AppList
		if err := conn.system.List(ctx, &list); err != nil {
			if conn.isLocal() {
				return nil, err
			}
			g.cfg.Logger.Warn("git webhook: cannot list apps", "cluster", st.name, "connection", gc.Name, "err", err)
			continue
		}
		for _, app := range matchingAppsIn(list.Items, gc, ev, keys) {
			out = append(out, hookApp{app: app, conn: conn})
		}
	}
	slices.SortFunc(out, func(a, b hookApp) int {
		return strings.Compare(a.app.Namespace+"/"+a.app.Name, b.app.Namespace+"/"+b.app.Name)
	})
	return out, nil
}

func matchingAppsIn(items []kwerftv1.App, gc *kwerftv1.GitConnection, ev git.Event, keys map[string]bool) []kwerftv1.App {
	var out []kwerftv1.App
	for _, app := range items {
		src := app.Spec.Source.Git
		if src == nil || src.Connection != gc.Name || !controllers.ProjectAllowed(gc, app.Namespace) || !app.DeletionTimestamp.IsZero() {
			continue
		}
		repo, err := git.ParseRepository(src.Repository)
		if err != nil || !keys[repo.Key()] {
			continue
		}
		branch := src.Branch
		if branch == "" {
			branch = "main"
		}
		if branch != ev.Branch {
			continue
		}
		out = append(out, app)
	}
	return out
}

// webhookBuild is builds.New with a name that is the same for every
// delivery of one event, so redeliveries do not build twice.
func webhookBuild(app *kwerftv1.App, ev git.Event, trigger string) (*kwerftv1.Build, error) {
	deploy := trigger == "push"
	if a := app.Spec.Source.Git.AutoDeploy; a != nil && !*a {
		deploy = false
	}
	branch := ev.Branch
	if trigger == "pull-request" {
		branch = ev.HeadBranch
	}
	b, err := builds.New(app, builds.Request{
		Commit:  builds.Commit{SHA: strings.ToLower(ev.Commit.SHA), Branch: branch, Message: ev.Commit.Message, Author: ev.Commit.Author},
		Trigger: trigger, RequestedBy: ev.Sender, PullRequest: ev.PullRequest, Deploy: deploy,
	})
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s/%s/%d/%s", app.UID, trigger, ev.PullRequest, b.Spec.Commit))
	b.Name = b.GenerateName + hex.EncodeToString(sum[:3])
	b.GenerateName = ""
	return b, nil
}
