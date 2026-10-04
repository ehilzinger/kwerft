package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/tools/remotecommand"
	utilexec "k8s.io/client-go/util/exec"

	"github.com/ehilzinger/kwerft/internal/controllers"
)

// Shells: a WebSocket between the browser's terminal (xterm.js) and `exec`
// in a container, run as the signed-in user.
//
// Handshake rules. Browsers attach cookies to a WebSocket handshake from any
// site, and neither CORS nor (reliably) SameSite stops a cross-site page
// from opening one, so a shell endpoint guarded by the cookie alone would let
// any website open a shell with a visitor's session (cross-site WebSocket
// hijacking). Before upgrading, the console therefore requires an Origin
// header naming this console's own host — browsers always send it on a
// WebSocket handshake and scripts cannot forge it — and a valid session
// cookie. A request without Origin is refused too: it did not come from a
// browser page, and the console has no other kind of client for shells yet.
//
// Protocol on the socket. Browser → console: binary messages are keystrokes;
// text messages are JSON control messages, {"type":"resize","cols":120,
// "rows":32}. Console → browser: binary messages are terminal output; text
// messages are JSON events: {"type":"started",...}, {"type":"error",
// "message":...} and finally {"type":"exit","reason":...,"code":...}.
//
// Limits: a session ends after idle without a keystroke, after maxDuration
// in any case, when the console session ends (sign-out, revocation), and
// when its recording reaches maxRecordingBytes. Each user may have a few
// shells open at once. Every session is audited (pod.exec at the start,
// pod.exec.end with duration and reason) and recorded (recording.go); with no
// place to record, shells are refused.

type shellLimits struct {
	idle         time.Duration // without a keystroke
	maxDuration  time.Duration
	sessionCheck time.Duration
	ping         time.Duration // WebSocket keepalive; a client silent for 2× is gone
	writeTimeout time.Duration
	maxMessage   int64
	debugStart   time.Duration // how long a debug container may take to start (image pull)
	debugPoll    time.Duration
}

var defaultShellLimits = shellLimits{
	idle: 15 * time.Minute, maxDuration: time.Hour, sessionCheck: 30 * time.Second,
	ping: 30 * time.Second, writeTimeout: 10 * time.Second, maxMessage: 64 << 10,
	debugStart: 2 * time.Minute, debugPoll: time.Second,
}

// shellCommands are the only commands a shell session runs. Each sets a TERM
// that matches xterm.js; "auto" prefers bash and falls back to sh. "debug"
// runs sh in a toolbox container added next to the target (api_debug.go).
var shellCommands = map[string][]string{
	"debug": {"sh", "-c", "TERM=xterm-256color; export TERM; exec sh"},
	"auto":  {"sh", "-c", "TERM=xterm-256color; export TERM; if command -v bash >/dev/null 2>&1; then exec bash; fi; exec sh"},
	"bash":  {"bash", "-c", "export TERM=xterm-256color; exec bash"},
	"sh":    {"sh", "-c", "TERM=xterm-256color; export TERM; exec sh"},
}

// wsOrigin enforces the handshake rules above before anything else runs.
func (p *podsAPI) wsOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			writeError(w, http.StatusBadRequest, "This endpoint only speaks WebSocket.")
			return
		}
		if !sameHostOrigin(r) {
			p.cfg.Logger.Warn("websocket refused: origin", "origin", r.Header.Get("Origin"), "host", r.Host, "ip", clientIP(r))
			writeError(w, http.StatusForbidden, "Cross-site WebSocket refused.")
			return
		}
		next(w, r)
	}
}

func sameHostOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && u.Host == r.Host && (u.Scheme == "https" || u.Scheme == "http")
}

func shouldFallBack(err error) bool {
	return httpstream.IsUpgradeFailure(err) || httpstream.IsHTTPSProxyError(err)
}

// shellOwner is what a shell's pod belongs to: a replica of an App, or the
// pod of a Task's run. Both share one implementation; resolve() and the
// recording's metadata tell them apart.
type shellOwner struct {
	kind string // app | task
	name string
}

func (o shellOwner) label() string {
	if o.kind == "task" {
		return controllers.LabelTask
	}
	return controllers.LabelApp
}

func (p *podsAPI) appShell(w http.ResponseWriter, r *http.Request) {
	p.openShell(w, r, shellOwner{kind: "app", name: r.PathValue("app")})
}

// taskShell opens a shell in the pod of a running Task, to look at what a job
// does while it runs. Finished runs have no container left to exec into.
func (p *podsAPI) taskShell(w http.ResponseWriter, r *http.Request) {
	p.openShell(w, r, shellOwner{kind: "task", name: r.PathValue("task")})
}

func (p *podsAPI) openShell(w http.ResponseWriter, r *http.Request, owner shellOwner) {
	project, pod := r.PathValue("project"), r.PathValue("pod")
	q := r.URL.Query()
	shell := q.Get("shell")
	if shell == "" {
		shell = "auto"
	}
	command, ok := shellCommands[shell]
	if !ok {
		invalid(w, "shell", "Pick sh, bash, auto or debug.")
		return
	}
	cols, rows := clampInt(q.Get("cols"), 80, 10, 500), clampInt(q.Get("rows"), 24, 4, 200)
	if p.rec == nil {
		writeError(w, http.StatusServiceUnavailable, "Shells are recorded, and this console has no place to keep recordings.")
		return
	}
	pr := r.Context().Value(ctxKey{}).(*principal)
	release := p.shells.acquire(pr.user.Email)
	if release == nil {
		writeError(w, http.StatusTooManyRequests, "Too many shells are open. Close another one and try again.")
		return
	}
	defer release()

	up := websocket.Upgrader{
		ReadBufferSize: 4 << 10, WriteBufferSize: 16 << 10,
		HandshakeTimeout: 10 * time.Second,
		CheckOrigin:      sameHostOrigin, // checked already; kept as a second lock
	}
	conn, err := up.Upgrade(w, r, nil)
	if err != nil {
		return // the upgrader answered with an HTTP error
	}
	p.running.Add(1)
	defer p.running.Add(-1)
	ws := &wsConn{c: conn, timeout: p.shell.writeTimeout}
	defer conn.Close()
	conn.SetReadLimit(p.shell.maxMessage)

	s := &shellSession{
		p: p, pr: pr, ws: ws, r: r,
		project: project, owner: owner, pod: pod, container: q.Get("container"),
		shell: shell, command: command, cols: cols, rows: rows,
	}
	s.run(r.Context())
}

func clampInt(v string, def, lo, hi int) int {
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return min(max(n, lo), hi)
}

// ---- the WebSocket ---------------------------------------------------------------

// wsConn serializes writes (gorilla allows one writer at a time).
type wsConn struct {
	c       *websocket.Conn
	timeout time.Duration
	mu      sync.Mutex
}

func (w *wsConn) write(kind int, data []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.SetWriteDeadline(time.Now().Add(w.timeout))
	return w.c.WriteMessage(kind, data)
}

func (w *wsConn) json(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return w.write(websocket.TextMessage, data)
}

func (w *wsConn) ping() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.c.WriteControl(websocket.PingMessage, nil, time.Now().Add(w.timeout))
}

func (w *wsConn) close(code int, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(code, text), time.Now().Add(w.timeout))
}

// ---- a session -------------------------------------------------------------------

type shellSession struct {
	p  *podsAPI
	pr *principal
	ws *wsConn
	r  *http.Request

	project, pod, container string
	owner                   shellOwner
	shell                   string
	debug                   string // the toolbox container of a debug shell
	command                 []string
	cols, rows              int

	rec *recording
}

// ended says why a session ended, for the browser, the audit log and the
// recording.
type ended struct {
	reason  string // exited | idle | maxDuration | signedOut | disconnected | recordingFull | error
	message string
	code    *int
}

func (s *shellSession) fail(message string) {
	_ = s.ws.json(map[string]string{"type": "error", "message": message})
	s.ws.close(websocket.ClosePolicyViolation, "")
}

func (s *shellSession) target() string { return s.project + "/" + s.pod }

func (s *shellSession) run(ctx context.Context) {
	p, email := s.p, s.pr.user.Email
	b, err := p.backend(s.r.Context(), s.pr)
	if err != nil {
		p.cfg.Logger.Error("shell: backend", "err", err)
		s.fail("Something went wrong on the server. Details are in the console logs.")
		return
	}
	pod, msg := s.resolve(ctx, b)
	if pod == nil {
		s.fail(msg)
		return
	}
	// Kubernetes decides; asking first gives a clear answer and an audit
	// entry instead of a failed stream.
	if ok, err := b.allowed(ctx, s.project, "create", "exec", s.pod); err != nil || !ok {
		if err == nil {
			p.audit(s.r, email, "pod.exec.denied", s.target(), "forbidden by Kubernetes RBAC")
			s.fail("Your role does not allow opening a shell.")
		} else {
			s.fail("Kubernetes did not answer: " + err.Error())
		}
		return
	}

	if s.shell == "debug" {
		debug, msg := s.startDebug(ctx, b, pod)
		if debug == "" {
			s.fail(msg)
			return
		}
		s.debug = debug
	}

	started := p.now()
	meta := recordingMeta{
		User: email, Project: s.project, Cluster: p.conn(s.r.Context()).name, Kind: s.owner.kind, Pod: s.pod, Container: s.container,
		Shell: s.shell, DebugContainer: s.debug, IP: clientIP(s.r), Started: started,
	}
	if s.owner.kind == "task" {
		meta.Task = s.owner.name
	} else {
		meta.App = s.owner.name
	}
	s.rec, err = p.rec.start(meta, s.cols, s.rows)
	if err != nil {
		p.cfg.Logger.Error("shell: cannot start the recording", "err", err)
		s.fail("Shell sessions are recorded, and the recording could not be started. Nothing was run.")
		return
	}
	detail := fmt.Sprintf("container %s, %s, recording %s", s.container, s.shell, s.rec.meta.ID)
	if s.debug != "" {
		detail = fmt.Sprintf("container %s, debug via %s, recording %s", s.container, s.debug, s.rec.meta.ID)
	}
	if s.owner.kind == "task" {
		detail += ", task " + s.owner.name
	}
	p.audit(s.r, email, "pod.exec", s.target(), detail)
	_ = s.ws.json(map[string]any{
		"type": "started", "pod": s.pod, "container": s.container, "shell": s.shell, "recording": s.rec.meta.ID,
		"idleSeconds": int(p.shell.idle.Seconds()), "maxSeconds": int(p.shell.maxDuration.Seconds()),
	})

	end, readerDone := s.loop(ctx, b)

	now := p.now()
	_ = s.rec.finish(now, end.reason, end.code)
	exit := map[string]any{"type": "exit", "reason": end.reason, "message": end.message}
	if end.code != nil {
		exit["code"] = *end.code
	}
	_ = s.ws.json(exit)
	s.ws.close(websocket.CloseNormalClosure, end.reason)
	_ = s.ws.c.Close() // ends the reader's ReadMessage
	<-readerDone
	detail = fmt.Sprintf("%s after %s, recording %s", end.reason, now.Sub(started).Round(time.Second), s.rec.meta.ID)
	if end.code != nil {
		detail += fmt.Sprintf(", exit code %d", *end.code)
	}
	p.audit(s.r, email, "pod.exec.end", s.target(), detail)
}

// resolve finds the pod as the user and checks that it belongs to the App (a
// replica) or the Task (its run, which must not have finished), and that the
// container runs; it returns nil and the reason otherwise.
func (s *shellSession) resolve(ctx context.Context, b podBackend) (*corev1.Pod, string) {
	ctx, cancel := context.WithTimeout(ctx, kubeTimeout)
	defer cancel()
	o := s.owner
	notFound := fmt.Sprintf("Replica %q not found. It may have been replaced; pick a current one.", s.pod)
	notOurs := fmt.Sprintf("Replica %q is not part of app %q.", s.pod, o.name)
	if o.kind == "task" {
		task, err := b.task(ctx, s.project, o.name)
		if err != nil {
			return nil, kubeMessage(err, targetNotFound("task", s.project, o.name))
		}
		if task.Status.Phase.Finished() {
			return nil, fmt.Sprintf("Run %q has finished (%s), so there is no container to open a shell in. Its logs stay in Task detail.",
				o.name, strings.ToLower(string(task.Status.Phase)))
		}
		notFound = fmt.Sprintf("Pod %q of run %q not found. A retry starts a new pod; pick the current one.", s.pod, o.name)
		notOurs = fmt.Sprintf("Pod %q is not part of run %q.", s.pod, o.name)
	} else if err := b.app(ctx, s.project, o.name); err != nil {
		return nil, kubeMessage(err, appNotFound(s.project, o.name))
	}
	pod, err := b.getPod(ctx, s.project, s.pod)
	if err != nil {
		return nil, kubeMessage(err, notFound)
	}
	if pod.Labels[o.label()] != o.name {
		return nil, notOurs
	}
	if s.container == "" && len(pod.Spec.Containers) > 0 {
		s.container = pod.Spec.Containers[0].Name
	}
	if !slices.ContainsFunc(pod.Spec.Containers, func(c corev1.Container) bool { return c.Name == s.container }) {
		return nil, fmt.Sprintf("There is no container %q in %s.", s.container, s.pod)
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != s.container {
			continue
		}
		if cs.State.Running == nil {
			why := "it is not running"
			switch st := cs.State; {
			case st.Waiting != nil && st.Waiting.Reason != "":
				why = "it is waiting (" + st.Waiting.Reason + ")"
			case st.Terminated != nil && o.kind == "task":
				why = fmt.Sprintf("the run's command has finished (exit code %d)", st.Terminated.ExitCode)
			case st.Terminated != nil && st.Terminated.Reason != "":
				why = "it has stopped (" + st.Terminated.Reason + ")"
			}
			return nil, fmt.Sprintf("Cannot open a shell in %s: %s.", s.container, why)
		}
		return pod, ""
	}
	return nil, fmt.Sprintf("Cannot open a shell in %s: it has not started yet.", s.container)
}

func kubeMessage(err error, notFound string) string {
	switch {
	case apierrors.IsNotFound(err):
		return notFound
	case apierrors.IsForbidden(err):
		return "Your role does not allow this."
	default:
		return "Kubernetes did not answer: " + apiMessage(err)
	}
}

// loop runs exec and the WebSocket until one of them, or a limit, ends the
// session. Exec has stopped when it returns; the input reader stops once the
// caller closes the connection (readerDone).
func (s *shellSession) loop(ctx context.Context, b podBackend) (ended, <-chan struct{}) {
	p := s.p
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()
	sizes := &sizeQueue{ch: make(chan remotecommand.TerminalSize, 4), done: ctx.Done()}
	sizes.push(s.cols, s.rows)
	keys := make(chan struct{}, 1)

	// Browser → container.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		s.readInput(ctx, cancel, stdinW, sizes, keys)
	}()

	// Container → browser (and the recording).
	out := &shellOutput{s: s, cancel: cancel}
	execErr := make(chan error, 1)
	go func() {
		container := s.container
		if s.debug != "" {
			container = s.debug
		}
		execErr <- b.exec(ctx, s.project, s.pod, &corev1.PodExecOptions{
			Container: container, Command: s.command, Stdin: true, Stdout: true, TTY: true,
		}, execStreams{stdin: stdinR, stdout: out, resize: sizes})
	}()

	idle := time.NewTimer(p.shell.idle)
	defer idle.Stop()
	deadline := time.NewTimer(p.shell.maxDuration)
	defer deadline.Stop()
	session := time.NewTicker(p.shell.sessionCheck)
	defer session.Stop()
	keepalive := time.NewTicker(p.shell.ping)
	defer keepalive.Stop()
	flush := time.NewTicker(time.Second)
	defer flush.Stop()

	var end ended
	for end.reason == "" {
		select {
		case err := <-execErr:
			end = exitOf(err, context.Cause(ctx))
			execErr = nil
		case <-ctx.Done():
			end = causeOf(context.Cause(ctx))
		case <-keys:
			idle.Reset(p.shell.idle)
		case <-idle.C:
			end = ended{reason: "idle", message: fmt.Sprintf("Closed after %s without input.", humanDuration(p.shell.idle))}
		case <-deadline.C:
			end = ended{reason: "maxDuration", message: fmt.Sprintf("Shell sessions end after %s.", humanDuration(p.shell.maxDuration))}
		case <-session.C:
			if !p.sessionAlive(ctx, s.pr) {
				end = ended{reason: "signedOut", message: "Your console session ended."}
			}
		case <-keepalive.C:
			if s.ws.ping() != nil {
				end = ended{reason: "disconnected"}
			}
		case <-flush.C:
			_ = s.rec.flush()
		}
	}
	// Stop exec (closing its streams hangs up the shell) and the reader.
	cancel(errSessionOver)
	_ = stdinW.CloseWithError(io.EOF)
	if execErr != nil {
		select {
		case <-execErr:
		case <-time.After(10 * time.Second):
			p.cfg.Logger.Warn("shell: exec did not stop within 10s", "pod", s.target())
		}
	}
	return end, readerDone
}

var (
	errSessionOver   = errors.New("session over")
	errDisconnected  = errors.New("browser disconnected")
	errOutputBlocked = errors.New("browser not reading")
)

func causeOf(cause error) ended {
	switch {
	case errors.Is(cause, errRecordingFull):
		return ended{reason: "recordingFull", message: fmt.Sprintf("The session reached the recording limit of %d MiB.", maxRecordingBytes>>20)}
	case errors.Is(cause, errDisconnected), errors.Is(cause, errOutputBlocked), errors.Is(cause, context.Canceled):
		return ended{reason: "disconnected"}
	default:
		return ended{reason: "error", message: cause.Error()}
	}
}

func exitOf(err, cause error) ended {
	if cause != nil {
		return causeOf(cause)
	}
	if err == nil {
		code := 0
		return ended{reason: "exited", code: &code}
	}
	var ee utilexec.ExitError
	if errors.As(err, &ee) {
		code := ee.ExitStatus()
		return ended{reason: "exited", code: &code, message: fmt.Sprintf("The shell exited with code %d.", code)}
	}
	if apierrors.IsForbidden(err) {
		return ended{reason: "error", message: "Your role does not allow opening a shell."}
	}
	if isNoShell(err) {
		return ended{reason: "noShell", message: "This image has no shell. Images built FROM scratch and distroless images contain only the app. " +
			"Open a debug shell instead: it adds a small toolbox container next to this one that sees its processes and network (the app's files are under /proc/1/root)."}
	}
	return ended{reason: "error", message: "The shell could not run: " + apiMessage(err)}
}

// readInput forwards keystrokes and resizes until the socket closes.
func (s *shellSession) readInput(ctx context.Context, cancel context.CancelCauseFunc, stdin *io.PipeWriter, sizes *sizeQueue, keys chan<- struct{}) {
	c := s.ws.c
	extend := func() { _ = c.SetReadDeadline(time.Now().Add(2 * s.p.shell.ping)) }
	extend()
	c.SetPongHandler(func(string) error { extend(); return nil })
	for {
		kind, data, err := c.ReadMessage()
		if err != nil {
			cancel(errDisconnected)
			return
		}
		extend()
		switch kind {
		case websocket.BinaryMessage:
			select {
			case keys <- struct{}{}:
			default:
			}
			if _, err := stdin.Write(data); err != nil {
				return // the shell is gone
			}
		case websocket.TextMessage:
			var msg struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &msg) != nil || msg.Type != "resize" {
				continue
			}
			cols, rows := min(max(msg.Cols, 10), 500), min(max(msg.Rows, 4), 200)
			sizes.push(cols, rows)
			if err := s.rec.resize(cols, rows, s.p.now()); errors.Is(err, errRecordingFull) {
				cancel(errRecordingFull)
			}
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// shellOutput sends container output to the browser and the recording. The
// recording comes first: output that cannot be recorded is not shown.
type shellOutput struct {
	s      *shellSession
	cancel context.CancelCauseFunc
}

func (o *shellOutput) Write(p []byte) (int, error) {
	if err := o.s.rec.output(p, o.s.p.now()); err != nil {
		o.cancel(err)
		return 0, err
	}
	if err := o.s.ws.write(websocket.BinaryMessage, p); err != nil {
		o.cancel(errOutputBlocked)
		return 0, err
	}
	return len(p), nil
}

// sizeQueue hands terminal sizes to remotecommand. Only the latest size
// matters, so a full queue drops the oldest.
type sizeQueue struct {
	ch   chan remotecommand.TerminalSize
	done <-chan struct{}
}

func (q *sizeQueue) push(cols, rows int) {
	size := remotecommand.TerminalSize{Width: uint16(cols), Height: uint16(rows)}
	for {
		select {
		case q.ch <- size:
			return
		default:
			select {
			case <-q.ch:
			default:
			}
		}
	}
}

func (q *sizeQueue) Next() *remotecommand.TerminalSize {
	select {
	case size := <-q.ch:
		return &size
	case <-q.done:
		return nil
	}
}
