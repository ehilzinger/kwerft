package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// Live logs.
//
// Transport: Server-Sent Events over a plain GET. Logs only flow from server
// to browser, so a WebSocket's second direction would be dead weight; SSE is
// ordinary HTTP (HTTP/2 through Traefik, no upgrade, no extra Origin rules),
// the session cookie and sameOrigin apply as to any request, errors before
// the stream are normal JSON responses, and curl can read it. The console
// reads it with fetch() rather than EventSource so it sees those errors and
// decides itself when to reconnect.
//
// Events:
//
//	event: start    {"pods":[...],"container":"app","follow":true,"limits":{...}}
//	event: line     {"pod":"web-7c9d8-x2kq","container":"app","ts":"2026-...Z","text":"...","truncated":true}
//	event: status   {"pod":"...","state":"streaming|waiting|ended|error","message":"..."}
//	event: dropped  {"lines":120}  lines skipped because the stream exceeded its rate
//	event: end      {"reason":"complete|idle|maxDuration|signedOut","message":"..."}
//	: ping          every heartbeat, to keep proxies from closing a quiet stream
//
// Kubernetes is always asked for timestamps: they order the lines of several
// replicas and let a resumed stream continue without repeating itself. The
// console shows or hides them; it does not refetch.
//
// Merging replicas: the backlog (tail/since) of every replica is fetched
// first and sorted by time, then each replica is followed from its last
// line, tagged with its pod. Replicas that appear later (rollouts, scaling)
// or restart (crash loops) are picked up by polling the pod list.
//
// Limits (logLimits): lines are cut at maxLine bytes; live lines above rate
// per second (burst allowed) are dropped and counted; at most maxPods
// replicas are merged; a stream ends after idle without a line or after
// maxDuration, and when the console session ends. Each user has a few
// concurrent streams. Backpressure: a slow browser blocks the writer, which
// stops reading Kubernetes, so memory stays bounded; a write that cannot
// finish within writeTimeout ends the stream.

type logLimits struct {
	maxLine      int   // bytes per line; longer lines are cut
	tailDefault  int64 // lines per replica when the request names none
	tailMax      int64
	sinceMax     time.Duration
	maxPods      int   // replicas merged into one stream
	backlogBytes int64 // for the initial fetch, shared by the replicas
	rate         float64
	burst        int
	idle         time.Duration
	maxDuration  time.Duration
	heartbeat    time.Duration
	poll         time.Duration // look for new or restarted replicas
	sessionCheck time.Duration
	writeTimeout time.Duration
}

var defaultLogLimits = logLimits{
	maxLine: 16 << 10, tailDefault: 500, tailMax: 5000, sinceMax: 30 * 24 * time.Hour,
	maxPods: 20, backlogBytes: 16 << 20, rate: 200, burst: 1000,
	idle: 15 * time.Minute, maxDuration: 2 * time.Hour, heartbeat: 20 * time.Second,
	poll: 5 * time.Second, sessionCheck: time.Minute, writeTimeout: 30 * time.Second,
}

// ---- handlers ----------------------------------------------------------------

func (p *podsAPI) appLogs(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("app")
	p.streamLogs(w, r, "app", project, name, appSelector(name))
}

func (p *podsAPI) taskLogs(w http.ResponseWriter, r *http.Request) {
	project, name := r.PathValue("project"), r.PathValue("task")
	p.streamLogs(w, r, "task", project, name, taskSelector(name))
}

type logQuery struct {
	pod, container   string
	follow, previous bool
	tail, since      int64
}

func boolParam(v string) bool { return v == "1" || v == "true" }

// parseLogQuery reads ?pod=&container=&follow=1&tail=500&since=3600&previous=1.
// since is in seconds.
func (lim logLimits) parseLogQuery(w http.ResponseWriter, r *http.Request) (logQuery, bool) {
	q := r.URL.Query()
	lq := logQuery{pod: q.Get("pod"), container: q.Get("container"), follow: boolParam(q.Get("follow")),
		previous: boolParam(q.Get("previous")), tail: lim.tailDefault}
	if v := q.Get("tail"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			invalid(w, "tail", "Enter a number of lines, 0 or more.")
			return lq, false
		}
		lq.tail = min(n, lim.tailMax)
	}
	if v := q.Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			invalid(w, "since", "Enter a time range in seconds, more than 0.")
			return lq, false
		}
		lq.since = min(n, int64(lim.sinceMax/time.Second))
	}
	if lq.previous {
		if lq.pod == "" {
			invalid(w, "previous", "Pick one replica to see the logs of its previous container.")
			return lq, false
		}
		lq.follow = false // a previous container has ended
	}
	return lq, true
}

func (p *podsAPI) streamLogs(w http.ResponseWriter, r *http.Request, kind, project, name string, sel labels.Selector) {
	lq, ok := p.logs.parseLogQuery(w, r)
	if !ok {
		return
	}
	pr := r.Context().Value(ctxKey{}).(*principal)
	b, err := p.backend(pr)
	if err != nil {
		p.internalError(w, r, err)
		return
	}
	target := appTarget(project, name)
	ctx, cancel := context.WithTimeout(r.Context(), kubeTimeout)
	defer cancel()
	if _, err := p.target(ctx, b, kind, project, name); err != nil {
		p.kubeError(w, r, pr, kind+".logs", target, targetNotFound(kind, project, name), err)
		return
	}
	if ok, err := b.allowed(ctx, project, "get", "log", lq.pod); err != nil {
		p.kubeError(w, r, pr, kind+".logs", target, targetNotFound(kind, project, name), err)
		return
	} else if !ok {
		p.audit(r, pr.user.Email, kind+".logs.denied", target, "forbidden by Kubernetes RBAC")
		writeError(w, http.StatusForbidden, "Your role does not allow reading logs.")
		return
	}
	podsOf := func(ctx context.Context) ([]corev1.Pod, error) {
		pods, err := b.listPods(ctx, project, sel)
		if lq.pod != "" {
			pods = slices.DeleteFunc(pods, func(pod corev1.Pod) bool { return pod.Name != lq.pod })
		}
		return pods, err
	}
	pods, err := podsOf(ctx)
	if err != nil {
		p.kubeError(w, r, pr, kind+".logs", target, targetNotFound(kind, project, name), err)
		return
	}
	if lq.pod != "" && len(pods) == 0 {
		writeError(w, http.StatusNotFound, fmt.Sprintf("Replica %q is not part of %s %q.", lq.pod, kind, name))
		return
	}
	if lq.container != "" && len(pods) > 0 && !slices.ContainsFunc(pods[0].Spec.Containers, func(c corev1.Container) bool { return c.Name == lq.container }) {
		invalid(w, "container", fmt.Sprintf("There is no container %q in this %s.", lq.container, kind))
		return
	}
	release := p.streams.acquire(pr.user.Email)
	if release == nil {
		writeError(w, http.StatusTooManyRequests, "Too many log streams are open. Close another log view and try again.")
		return
	}
	defer release()
	p.running.Add(1)
	defer p.running.Add(-1)

	s := &logStream{
		b: b, ns: project, pods: podsOf, q: lq, lim: p.logs,
		alive: func(ctx context.Context) bool { return p.sessionAlive(ctx, pr) },
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // no proxy buffering
	w.WriteHeader(http.StatusOK)
	s.out = &sseWriter{w: w, rc: http.NewResponseController(w), timeout: p.logs.writeTimeout}
	s.run(r.Context(), pods)
}

// ---- the stream ----------------------------------------------------------------

type logLine struct {
	Pod       string    `json:"pod"`
	Container string    `json:"container"`
	TS        string    `json:"ts,omitempty"`
	Text      string    `json:"text"`
	Truncated bool      `json:"truncated,omitempty"`
	at        time.Time // parsed TS; zero when the line had none
}

type logStatus struct {
	Pod     string `json:"pod"`
	State   string `json:"state"` // streaming | waiting | ended | error
	Message string `json:"message,omitempty"`
}

type logStream struct {
	b     podBackend
	ns    string
	pods  func(context.Context) ([]corev1.Pod, error)
	q     logQuery
	lim   logLimits
	out   *sseWriter
	alive func(context.Context) bool

	dropped int
}

// errClientGone means the browser went away or stopped reading.
var errClientGone = errors.New("client gone")

func (s *logStream) container(pod *corev1.Pod) string {
	if s.q.container != "" {
		return s.q.container
	}
	if c := pod.Annotations["kubectl.kubernetes.io/default-container"]; c != "" {
		return c
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// hasLogs reports whether the container has started at least once (or, for
// previous=1, has a previous instance), so a log request can succeed. The
// reason comes back when it cannot.
func (s *logStream) hasLogs(pod *corev1.Pod) (bool, string) {
	name := s.container(pod)
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != name {
			continue
		}
		if s.q.previous {
			if cs.LastTerminationState.Terminated != nil {
				return true, ""
			}
			return false, "This container has not restarted, so there is no previous container."
		}
		switch {
		case cs.State.Running != nil, cs.State.Terminated != nil:
			return true, ""
		case cs.State.Waiting != nil && cs.LastTerminationState.Terminated != nil:
			return true, "" // between restarts: the last run's logs are still there
		case cs.State.Waiting != nil:
			return false, "Waiting: " + cs.State.Waiting.Reason
		}
	}
	return false, "Waiting for the container to start."
}

func (s *logStream) run(ctx context.Context, pods []corev1.Pod) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if len(pods) > s.lim.maxPods {
		pods = pods[:s.lim.maxPods]
	}
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	container := ""
	if len(pods) > 0 {
		container = s.container(&pods[0])
	}
	if s.out.event("start", map[string]any{
		"pods": names, "container": container, "follow": s.q.follow, "previous": s.q.previous,
		"limits": map[string]any{"tail": s.q.tail, "maxLine": s.lim.maxLine, "linesPerSecond": s.lim.rate, "maxPods": s.lim.maxPods},
	}) != nil {
		return
	}

	started := time.Now()
	last, err := s.backlog(ctx, pods)
	if err != nil {
		return
	}
	if !s.q.follow {
		_ = s.end("complete", "")
		return
	}
	s.follow(ctx, pods, last, started)
}

// backlog sends the existing lines of every replica, merged by time, and
// returns the time of each replica's last line.
func (s *logStream) backlog(ctx context.Context, pods []corev1.Pod) (map[string]time.Time, error) {
	type result struct {
		lines  []logLine
		status *logStatus
	}
	results := make([]result, len(pods))
	// The backlog is sorted in memory, so its size is bounded in total.
	limit := max(s.lim.backlogBytes/int64(max(len(pods), 1)), 256<<10)
	var wg sync.WaitGroup
	for i := range pods {
		pod := &pods[i]
		if ok, why := s.hasLogs(pod); !ok {
			results[i].status = &logStatus{Pod: pod.Name, State: "waiting", Message: why}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			opts := s.options(pod)
			opts.TailLines = &s.q.tail
			if s.q.since > 0 {
				opts.SinceSeconds = &s.q.since
			}
			opts.LimitBytes = &limit
			lines, err := s.fetch(ctx, pod.Name, opts)
			results[i].lines = lines
			if err != nil && ctx.Err() == nil {
				results[i].status = statusFor(pod.Name, err)
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, errClientGone
	}

	var all []logLine
	for _, res := range results {
		if res.status != nil {
			if s.out.event("status", res.status) != nil {
				return nil, errClientGone
			}
		}
		all = append(all, res.lines...)
	}
	// Stable: lines of one replica keep their order whatever their stamps.
	slices.SortStableFunc(all, func(x, y logLine) int { return x.at.Compare(y.at) })
	last := map[string]time.Time{}
	for i, ln := range all {
		if s.out.event("line", ln) != nil {
			return nil, errClientGone
		}
		if ln.at.After(last[ln.Pod]) {
			last[ln.Pod] = ln.at
		}
		if i%500 == 499 && s.out.flush() != nil {
			return nil, errClientGone
		}
	}
	if s.out.flush() != nil {
		return nil, errClientGone
	}
	return last, nil
}

func (s *logStream) options(pod *corev1.Pod) *corev1.PodLogOptions {
	return &corev1.PodLogOptions{Container: s.container(pod), Timestamps: true, Previous: s.q.previous}
}

// fetch reads a finished (non-follow) log request into memory. LimitBytes
// bounds it.
func (s *logStream) fetch(ctx context.Context, pod string, opts *corev1.PodLogOptions) ([]logLine, error) {
	rc, err := s.b.logs(ctx, s.ns, pod, opts)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	var lines []logLine
	err = readLines(rc, s.lim.maxLine, func(raw []byte, cut bool) bool {
		lines = append(lines, parseLogLine(pod, opts.Container, raw, cut))
		return true
	})
	return lines, err
}

func statusFor(pod string, err error) *logStatus {
	st := &logStatus{Pod: pod, State: "error", Message: "Kubernetes did not return the logs: " + err.Error()}
	switch {
	case apierrors.IsForbidden(err):
		st.Message = "Your role does not allow reading these logs."
	case apierrors.IsNotFound(err):
		st.State, st.Message = "ended", "The replica is gone."
	case apierrors.IsBadRequest(err):
		// "container ... is waiting to start", "previous terminated container not found"
		st.State, st.Message = "waiting", apiMessage(err)
	}
	return st
}

func apiMessage(err error) string {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		return status.Status().Message
	}
	return err.Error()
}

type podDone struct {
	pod string
	err error
}

// follow streams every replica live until the client leaves or a limit ends
// the stream. All writes to the client happen in this goroutine.
func (s *logStream) follow(ctx context.Context, pods []corev1.Pod, last map[string]time.Time, backlogStarted time.Time) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait() // every reader has closed its stream before the handler returns
	}()

	lines := make(chan logLine, 256)
	done := make(chan podDone, s.lim.maxPods+1)
	active := map[string]bool{}
	denied := map[string]bool{}
	reported := map[string]string{} // last status sent per pod, to not repeat it

	start := func(pod *corev1.Pod) {
		if active[pod.Name] || denied[pod.Name] || len(active) >= s.lim.maxPods {
			return
		}
		if ok, why := s.hasLogs(pod); !ok {
			if reported[pod.Name] != why {
				reported[pod.Name] = why
				_ = s.out.event("status", logStatus{Pod: pod.Name, State: "waiting", Message: why})
			}
			return
		}
		opts := s.options(pod)
		opts.Follow = true
		after, seen := last[pod.Name]
		switch {
		case seen:
			// Resume after the last line sent. SinceTime has second
			// precision, so lines up to `after` come again and are skipped.
			opts.SinceTime = &metav1.Time{Time: after}
		case pod.CreationTimestamp.Time.Before(backlogStarted):
			// Known since the backlog, which had nothing: anything from
			// shortly before it started is new.
			since := metav1.NewTime(backlogStarted.Add(-2 * time.Second))
			opts.SinceTime = &since
		default:
			opts.TailLines = &s.q.tail // a new replica: from its start
		}
		active[pod.Name] = true
		if reported[pod.Name] != "streaming" {
			reported[pod.Name] = "streaming"
			_ = s.out.event("status", logStatus{Pod: pod.Name, State: "streaming"})
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.tailOne(ctx, pod.Name, opts, after, lines)
			select {
			case done <- podDone{pod.Name, err}:
			case <-ctx.Done():
			}
		}()
	}
	for i := range pods {
		start(&pods[i])
	}
	if s.out.flush() != nil {
		return
	}

	bucket := newTokenBucket(s.lim.rate, s.lim.burst)
	idle := time.NewTimer(s.lim.idle)
	defer idle.Stop()
	deadline := time.NewTimer(s.lim.maxDuration)
	defer deadline.Stop()
	heartbeat := time.NewTicker(s.lim.heartbeat)
	defer heartbeat.Stop()
	poll := time.NewTicker(s.lim.poll)
	defer poll.Stop()
	session := time.NewTicker(s.lim.sessionCheck)
	defer session.Stop()
	report := time.NewTicker(time.Second)
	defer report.Stop()

	for {
		var err error
		select {
		case <-ctx.Done():
			return
		case ln := <-lines:
			if ln.at.After(last[ln.Pod]) {
				last[ln.Pod] = ln.at
			}
			if !bucket.take(time.Now()) {
				s.dropped++
				continue
			}
			err = s.out.event("line", ln)
			idle.Reset(s.lim.idle) // Go ≥ 1.23 timers: no stale tick to drain
		case d := <-done:
			delete(active, d.pod)
			st := logStatus{Pod: d.pod, State: "ended", Message: "The container stopped. Waiting for it to run again."}
			if d.err != nil {
				st = *statusFor(d.pod, d.err)
				denied[d.pod] = apierrors.IsForbidden(d.err)
			}
			reported[d.pod] = st.Message
			err = s.out.event("status", st)
		case <-poll.C:
			current, perr := s.pods(ctx)
			if perr != nil {
				continue // try again on the next tick
			}
			for i := range current {
				start(&current[i])
			}
		case <-heartbeat.C:
			err = s.out.comment("ping")
		case <-report.C:
			if s.dropped > 0 {
				err = s.out.event("dropped", map[string]int{"lines": s.dropped})
				s.dropped = 0
			}
		case <-session.C:
			if !s.alive(ctx) {
				_ = s.end("signedOut", "Your session ended. Sign in again to continue.")
				return
			}
		case <-idle.C:
			_ = s.end("idle", fmt.Sprintf("No new lines for %s, so the stream stopped.", humanDuration(s.lim.idle)))
			return
		case <-deadline.C:
			_ = s.end("maxDuration", fmt.Sprintf("Log streams stop after %s.", humanDuration(s.lim.maxDuration)))
			return
		}
		// Flush once the burst is written, not after every line.
		if err == nil && len(lines) == 0 {
			err = s.out.flush()
		}
		if err != nil {
			return
		}
	}
}

// tailOne follows one replica, sending its lines after `after`.
func (s *logStream) tailOne(ctx context.Context, pod string, opts *corev1.PodLogOptions, after time.Time, lines chan<- logLine) error {
	rc, err := s.b.logs(ctx, s.ns, pod, opts)
	if err != nil {
		return err
	}
	defer rc.Close()
	// A blocked Read only returns once the body is closed.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer stop()
	err = readLines(rc, s.lim.maxLine, func(raw []byte, cut bool) bool {
		ln := parseLogLine(pod, opts.Container, raw, cut)
		if !after.IsZero() && !ln.at.IsZero() && !ln.at.After(after) {
			return true // sent before
		}
		select {
		case lines <- ln:
			return true
		case <-ctx.Done():
			return false
		}
	})
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func (s *logStream) end(reason, message string) error {
	if s.dropped > 0 {
		_ = s.out.event("dropped", map[string]int{"lines": s.dropped})
	}
	if err := s.out.event("end", map[string]string{"reason": reason, "message": message}); err != nil {
		return err
	}
	return s.out.flush()
}

// readLines calls fn for every line of r (without the line break), cutting
// lines at maxLine bytes. It stops when fn returns false.
func readLines(r io.Reader, maxLine int, fn func(line []byte, truncated bool) bool) error {
	br := bufio.NewReaderSize(r, 32<<10)
	line := make([]byte, 0, 256)
	truncated := false
	for {
		chunk, err := br.ReadSlice('\n')
		complete := err == nil
		if complete {
			chunk = bytes.TrimSuffix(chunk[:len(chunk)-1], []byte("\r"))
		}
		if room := maxLine - len(line); len(chunk) > room {
			line = append(line, chunk[:max(room, 0)]...)
			truncated = true
		} else {
			line = append(line, chunk...)
		}
		switch {
		case complete:
			if !fn(line, truncated) {
				return nil
			}
			line, truncated = line[:0], false
		case errors.Is(err, bufio.ErrBufferFull):
			// a long line: keep reading it (and dropping what does not fit)
		default:
			if len(line) > 0 {
				fn(line, truncated)
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// parseLogLine splits the timestamp Kubernetes puts in front of every line
// when asked for timestamps ("2026-10-04T09:14:02.381234567Z text").
func parseLogLine(pod, container string, raw []byte, truncated bool) logLine {
	ln := logLine{Pod: pod, Container: container, Truncated: truncated}
	text := raw
	if i := bytes.IndexByte(raw, ' '); i > 0 {
		if at, err := time.Parse(time.RFC3339Nano, string(raw[:i])); err == nil {
			ln.at, ln.TS, text = at, at.UTC().Format(time.RFC3339Nano), raw[i+1:]
		}
	} else if at, err := time.Parse(time.RFC3339Nano, string(raw)); err == nil {
		ln.at, ln.TS, text = at, at.UTC().Format(time.RFC3339Nano), nil // an empty line
	}
	ln.Text = string(text)
	return ln
}

// ---- server-sent events ------------------------------------------------------

type sseWriter struct {
	w       io.Writer
	rc      *http.ResponseController
	timeout time.Duration
}

func (s *sseWriter) event(name string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", name, data); err != nil {
		return errClientGone
	}
	return nil
}

func (s *sseWriter) comment(text string) error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	if _, err := fmt.Fprintf(s.w, ": %s\n\n", text); err != nil {
		return errClientGone
	}
	return s.flush()
}

func (s *sseWriter) flush() error {
	_ = s.rc.SetWriteDeadline(time.Now().Add(s.timeout))
	if err := s.rc.Flush(); err != nil {
		return errClientGone
	}
	return nil
}

// ---- rate ------------------------------------------------------------------------

// tokenBucket is a token bucket: rate tokens per second, at most burst saved up.
type tokenBucket struct {
	rate, burst, tokens float64
	last                time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{rate: rate, burst: float64(burst), tokens: float64(burst)}
}

func (b *tokenBucket) take(now time.Time) bool {
	if !b.last.IsZero() {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func humanDuration(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		if d == time.Hour {
			return "1 hour"
		}
		return fmt.Sprintf("%d hours", d/time.Hour)
	case d >= time.Minute && d%time.Minute == 0:
		if d == time.Minute {
			return "1 minute"
		}
		return fmt.Sprintf("%d minutes", d/time.Minute)
	default:
		return d.String()
	}
}
