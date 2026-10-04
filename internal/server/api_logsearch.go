package server

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kwerftv1 "github.com/ehilzinger/kwerft/api/v1alpha1"
	"github.com/ehilzinger/kwerft/internal/logs"
)

// Log search over VictoriaLogs (docs/phase3.md, W2): the history of every
// container's output, beyond the live pods that api_logs.go streams.
//
//	GET /api/v1/logs?query=<LogsQL>&project=&app=&level=&since=&until=&limit=&platform=1
//	    → {entries: [{time, namespace, project?, app?, build?, task?, pod, container, stream, level?, line, truncated?}],
//	       truncated, from, to}
//	GET /api/v1/logs/tail?query=&project=&app=&level=&platform=1
//	    SSE: start, line (an entry), dropped, end, ": ping" — as the pod log streams
//
// VictoriaLogs is read with the console's own identity; what a user sees is
// decided here (see internal/logs for how the scope is enforced):
//
//   - the scope is the project namespaces the user reaches (scope.go):
//     Projects with access Team or listing the user as a member, whose
//     namespace carries the kwerft.dev/project label for that project (read
//     with the console's identity), minus platform namespaces;
//   - project= narrows it to one of those; app= additionally needs the
//     user's impersonated get of the App;
//   - platform=1 lets owners and admins search every namespace (kube-system,
//     kwerft-system, kwerft-builds, ...); others get 403;
//   - query= is the user's LogsQL filter. It can only narrow the scope.
//
// Reads are not audited, like the live log streams. since/until take
// seconds or a duration ago (3600, 15m) or an RFC 3339 time; the default
// range is the last hour. until is inclusive, so "load older" passes the
// oldest time it has and drops the lines it already shows.

const (
	searchDefaultLimit = 200
	searchMaxLimit     = 1000
	searchDefaultSince = time.Hour
)

type logSearchAPI struct {
	*api
	// projectNamespaces are the project namespaces a user may read logs of;
	// allNamespaces every namespace (platform search). Tests swap both.
	projectNamespaces func(ctx context.Context, pr *principal) ([]string, error)
	allNamespaces     func(ctx context.Context) ([]string, error)
	lim               logLimits
	streams           *slots
	// running counts open tails, so tests can wait for cleanup.
	running atomic.Int32
}

func (a *api) registerLogSearch(mux *http.ServeMux) {
	s := &logSearchAPI{api: a, lim: defaultLogLimits, streams: newSlots(6, 200)}
	s.projectNamespaces = s.kubeProjectNamespaces
	s.allNamespaces = s.kubeAllNamespaces
	if a.cfg.logsHook != nil {
		a.cfg.logsHook(s)
	}
	// Application output: refused from other sites, like the log streams.
	read := func(h http.HandlerFunc) http.HandlerFunc { return a.sameOrigin(a.requireUser(a.requireKube(h))) }
	mux.HandleFunc("GET /api/v1/logs", read(s.search))
	mux.HandleFunc("GET /api/v1/logs/tail", read(s.tail))
}

// platformNamespace: never part of a project scope, whatever its labels say.
func platformNamespace(ns string) bool {
	return kwerftv1.IsReservedProjectName(ns)
}

func (s *logSearchAPI) kubeProjectNamespaces(ctx context.Context, pr *principal) ([]string, error) {
	scope, err := s.projectScope(ctx, pr)
	if err != nil {
		return nil, err
	}
	return scope.namespaces(), nil
}

func (s *logSearchAPI) kubeAllNamespaces(ctx context.Context) ([]string, error) {
	cs, err := s.cfg.Kube.Self()
	if err != nil {
		return nil, err
	}
	list, err := cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		out = append(out, ns.Name)
	}
	return out, nil
}

// searchRequest reads the shared parameters and works out the scope. On
// false the response has been written.
func (s *logSearchAPI) searchRequest(w http.ResponseWriter, r *http.Request, withRange bool) (logs.Request, map[string]bool, bool) {
	q := r.URL.Query()
	pr := r.Context().Value(ctxKey{}).(*principal)
	project, app := q.Get("project"), q.Get("app")
	req := logs.Request{Query: q.Get("query"), Level: q.Get("level")}
	if err := logs.CheckQuery(req.Query, s.vlogs.Limits.MaxQuery); err != nil {
		invalid(w, "query", err.Error())
		return req, nil, false
	}
	if !logs.ValidLevel(req.Level) {
		invalid(w, "level", "Pick error, warn or all levels.")
		return req, nil, false
	}
	if withRange {
		now := s.now()
		req.Start, req.End, req.Limit = now.Add(-searchDefaultSince), now, searchDefaultLimit
		if v := q.Get("since"); v != "" {
			t, err := logs.ParseTime(v, now)
			if err != nil {
				invalid(w, "since", err.Error())
				return req, nil, false
			}
			req.Start = t
		}
		if v := q.Get("until"); v != "" {
			t, err := logs.ParseTime(v, now)
			if err != nil {
				invalid(w, "until", err.Error())
				return req, nil, false
			}
			req.End = t
		}
		if !req.Start.Before(req.End) {
			invalid(w, "since", "The start of the time range must be before its end.")
			return req, nil, false
		}
		if max := s.vlogs.Limits.MaxRange; req.End.Sub(req.Start) > max {
			invalid(w, "since", fmt.Sprintf("Search at most %s at a time.", humanDuration(max)))
			return req, nil, false
		}
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				invalid(w, "limit", "Enter a number of lines, 1 or more.")
				return req, nil, false
			}
			req.Limit = min(n, searchMaxLimit)
		}
	}
	if app != "" && project == "" {
		invalid(w, "project", "Pick the app's project.")
		return req, nil, false
	}

	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	namespaces, err := s.projectNamespaces(ctx, pr)
	if err != nil {
		s.kubeError(w, r, pr, "logs.search", "", "No projects found.", err)
		return req, nil, false
	}
	projects := map[string]bool{}
	for _, ns := range namespaces {
		projects[ns] = true
	}
	switch {
	case project != "":
		if !projects[project] {
			writeError(w, http.StatusNotFound, fmt.Sprintf("Project %q not found.", project))
			return req, nil, false
		}
		req.Scope.Namespaces = []string{project}
		if app != "" {
			c, err := s.cfg.Kube.For(pr.user.Email, pr.user.Role)
			if err != nil {
				s.internalError(w, r, err)
				return req, nil, false
			}
			if err := c.Get(ctx, types.NamespacedName{Namespace: project, Name: app}, &kwerftv1.App{}); err != nil {
				s.kubeError(w, r, pr, "logs.search", appTarget(project, app), appNotFound(project, app), err)
				return req, nil, false
			}
			req.Scope.Fields = map[string]string{logs.FieldApp: app}
		}
	case boolParam(q.Get("platform")):
		if pr.user.Role != "owner" && pr.user.Role != "admin" {
			writeError(w, http.StatusForbidden, "Only owners and admins may search the platform's logs.")
			return req, nil, false
		}
		all, err := s.allNamespaces(ctx)
		if err != nil {
			s.internalError(w, r, err)
			return req, nil, false
		}
		req.Scope.Namespaces = all
	default:
		req.Scope.Namespaces = namespaces
	}
	return req, projects, true
}

// logsError answers a failed VictoriaLogs request.
func (a *api) logsError(w http.ResponseWriter, r *http.Request, err error) {
	var qe *logs.QueryError
	switch {
	case errors.As(err, &qe):
		invalid(w, "query", qe.Msg)
	case errors.Is(err, logs.ErrUnavailable):
		a.cfg.Logger.Warn("log search failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "Log history is not available right now: VictoriaLogs did not answer. Live logs of running pods still work.")
	case errors.Is(err, context.Canceled):
		// the browser went away
	default:
		a.internalError(w, r, err)
	}
}

// withProject fills in the project of lines from project namespaces whose
// pods lack the label (made outside Kwerft).
func withProject(e *logs.Entry, projects map[string]bool) {
	if e.Project == "" && projects[e.Namespace] {
		e.Project = e.Namespace
	}
}

type searchJSON struct {
	Entries   []logs.Entry `json:"entries"`
	Truncated bool         `json:"truncated"`
	From      time.Time    `json:"from"`
	To        time.Time    `json:"to"`
}

func (s *logSearchAPI) search(w http.ResponseWriter, r *http.Request) {
	req, projects, ok := s.searchRequest(w, r, true)
	if !ok {
		return
	}
	res, err := s.vlogs.Query(r.Context(), req)
	if err != nil {
		s.logsError(w, r, err)
		return
	}
	out := searchJSON{Entries: res.Entries, Truncated: res.Truncated, From: req.Start.UTC(), To: req.End.UTC()}
	if out.Entries == nil {
		out.Entries = []logs.Entry{}
	}
	for i := range out.Entries {
		withProject(&out.Entries[i], projects)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, out)
}

// tail follows new lines as Server-Sent Events, with the limits of the pod
// log streams: a rate (excess lines dropped and counted), an idle timeout, a
// maximum duration, a heartbeat, and the end of the console session.
func (s *logSearchAPI) tail(w http.ResponseWriter, r *http.Request) {
	req, projects, ok := s.searchRequest(w, r, false)
	if !ok {
		return
	}
	pr := r.Context().Value(ctxKey{}).(*principal)
	if len(req.Scope.Namespaces) == 0 {
		writeError(w, http.StatusNotFound, "There are no projects to read logs from.")
		return
	}
	release := s.streams.acquire(pr.user.Email)
	if release == nil {
		writeError(w, http.StatusTooManyRequests, "Too many log streams are open. Close another log view and try again.")
		return
	}
	defer release()
	s.running.Add(1)
	defer s.running.Add(-1)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	t, err := s.vlogs.Tail(ctx, req)
	if err != nil {
		s.logsError(w, r, err)
		return
	}
	defer t.Close()
	lines := make(chan logs.Entry, 256)
	failed := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			e, err := t.Next()
			if err != nil {
				failed <- err
				return
			}
			select {
			case lines <- e:
			case <-ctx.Done():
				return
			}
		}
	})
	defer func() {
		cancel()
		_ = t.Close() // unblocks Next
		wg.Wait()
	}()

	out := startSSE(w, s.lim.writeTimeout)
	if out.event("start", map[string]any{"limits": map[string]any{
		"maxLine": s.vlogs.Limits.MaxLine, "linesPerSecond": s.lim.rate, "maxSeconds": int(s.lim.maxDuration / time.Second),
	}}) != nil || out.flush() != nil {
		return
	}

	dropped := 0
	end := func(reason, message string) {
		if dropped > 0 {
			_ = out.event("dropped", map[string]int{"lines": dropped})
		}
		if out.event("end", map[string]string{"reason": reason, "message": message}) == nil {
			_ = out.flush()
		}
	}
	bucket := newTokenBucket(s.lim.rate, s.lim.burst)
	idle := time.NewTimer(s.lim.idle)
	defer idle.Stop()
	deadline := time.NewTimer(s.lim.maxDuration)
	defer deadline.Stop()
	heartbeat := time.NewTicker(s.lim.heartbeat)
	defer heartbeat.Stop()
	session := time.NewTicker(s.lim.sessionCheck)
	defer session.Stop()
	report := time.NewTicker(time.Second)
	defer report.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case e := <-lines:
			if !bucket.take(time.Now()) {
				dropped++
				continue
			}
			withProject(&e, projects)
			err = out.event("line", e)
			idle.Reset(s.lim.idle)
		case ferr := <-failed:
			if ctx.Err() != nil {
				return
			}
			s.cfg.Logger.Warn("log tail ended", "err", ferr)
			end("error", "VictoriaLogs ended the stream. Start it again to continue.")
			return
		case <-heartbeat.C:
			err = out.comment("ping")
		case <-report.C:
			if dropped > 0 {
				err = out.event("dropped", map[string]int{"lines": dropped})
				dropped = 0
			}
		case <-session.C:
			if !s.streamSessionAlive(ctx, pr) {
				end("signedOut", "Your session ended. Sign in again to continue.")
				return
			}
		case <-idle.C:
			end("idle", fmt.Sprintf("No new lines for %s, so the stream stopped.", humanDuration(s.lim.idle)))
			return
		case <-deadline.C:
			end("maxDuration", fmt.Sprintf("Log streams stop after %s.", humanDuration(s.lim.maxDuration)))
			return
		}
		if err == nil && len(lines) == 0 {
			err = out.flush()
		}
		if err != nil {
			return
		}
	}
}

// streamSessionAlive: the session still exists and the role is unchanged.
func (a *api) streamSessionAlive(ctx context.Context, pr *principal) bool {
	_, u, err := a.store.SessionByHash(ctx, pr.idHash, a.now())
	return err == nil && u.Role == pr.user.Role
}

// ---- history of Task and Build logs ------------------------------------------------

// historyLog reads a finished Task's or Build's log from VictoriaLogs once
// its pods are gone. The caller has checked that the user may read that log
// and confines scope to its pods. nil means nothing to show: no lines, or
// VictoriaLogs did not answer; the caller then answers as it did before.
func (a *api) historyLog(ctx context.Context, scope logs.Scope, start, end time.Time, tail int64, container string) *logs.Result {
	if tail <= 0 {
		tail = defaultLogLimits.tailDefault
	}
	if container != "" {
		scope.Fields = maps.Clone(scope.Fields)
		if scope.Fields == nil {
			scope.Fields = map[string]string{}
		}
		scope.Fields[logs.FieldContainer] = container
	}
	if max := a.vlogs.Limits.MaxRange; end.Sub(start) > max {
		start = end.Add(-max)
	}
	res, err := a.vlogs.Query(ctx, logs.Request{Scope: scope, Start: start, End: end, Limit: int(tail)})
	if err != nil {
		if ctx.Err() == nil {
			a.cfg.Logger.Warn("log history unavailable", "scope", scope, "err", err)
		}
		return nil
	}
	if len(res.Entries) == 0 {
		return nil
	}
	return &res
}

// sendHistory answers a log stream request with lines from history, as the
// events of a finished pod log stream: start (with "source": "history" and
// a message saying so), the lines, and end "complete".
func sendHistory(w http.ResponseWriter, timeout time.Duration, res *logs.Result, container string) {
	out := startSSE(w, timeout)
	var pods []string
	for _, e := range res.Entries {
		if !slices.Contains(pods, e.Pod) {
			pods = append(pods, e.Pod)
		}
	}
	msg := "From log history: the pod has been removed, so these lines come from VictoriaLogs."
	if res.Truncated {
		msg = fmt.Sprintf("From log history: the pod has been removed, so these are its last %d lines from VictoriaLogs.", len(res.Entries))
	}
	if out.event("start", map[string]any{
		"pods": pods, "container": container, "follow": false, "previous": false, "source": "history", "message": msg,
	}) != nil {
		return
	}
	for i, e := range res.Entries {
		ln := logLine{Pod: e.Pod, Container: e.Container, TS: e.Time.Format(time.RFC3339Nano), Text: e.Line, Truncated: e.Truncated}
		if out.event("line", ln) != nil {
			return
		}
		if i%500 == 499 && out.flush() != nil {
			return
		}
	}
	if out.event("end", map[string]string{"reason": "complete", "message": msg}) == nil {
		_ = out.flush()
	}
}

// startSSE writes the headers of an event stream.
func startSSE(w http.ResponseWriter, timeout time.Duration) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // no proxy buffering
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w), timeout: timeout}
}

// historyRange is when a Task's or Build's pods ran: from shortly before it
// was created to a while after it finished (logs arrive with a delay).
func historyRange(created metav1.Time, finished *metav1.Time, now time.Time) (time.Time, time.Time) {
	end := now
	if finished != nil && finished.Add(10*time.Minute).Before(now) {
		end = finished.Add(10 * time.Minute)
	}
	return created.Add(-time.Minute), end
}
