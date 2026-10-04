package server

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Shell recordings, in the asciinema v2 format
// (https://docs.asciinema.org/manual/asciicast/v2/): a JSON header line, then
// one JSON array per event, [seconds, code, data], where code is "o" for
// output, "r" for a resize ("80x24") and "m" for a marker. Any asciinema
// player replays them.
//
// What is recorded: everything the terminal showed (output, which includes
// the echo of what was typed) and resizes. Keystrokes ("i" events) are not:
// what a terminal does not echo is mostly passwords and tokens typed at
// prompts (sudo, psql, mysql -p), and a recording of them would turn the
// recordings directory into a store of secrets. The output alone still
// shows every command that was run. Output can contain secrets too (`env`,
// `cat` of a key), so recordings are files only the console can read (0600
// in a 0700 directory on its data volume), only owners and admins may list
// and download or play them, every download and playback is audited, and
// they are deleted after
// recordingRetention.
//
// Next to each <id>.cast is <id>.json with who, where, when and how the
// session ended; it is written when the session starts and again when it
// ends, so a session cut short by a crash still shows up.

const (
	recordingRetention = 90 * 24 * time.Hour
	// maxRecordingBytes ends a session whose recording grows beyond it: no
	// shell runs unrecorded, and the data volume must not fill up.
	maxRecordingBytes = 64 << 20
)

var errRecordingFull = errors.New("recording size limit reached")

var recordingIDRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

type recorder struct {
	dir       string
	retention time.Duration
	maxBytes  int64
	log       *slog.Logger

	mu        sync.Mutex
	lastPrune time.Time
	live      map[string]bool // recordings of sessions still running
}

// recordingMeta is the sidecar <id>.json and what the list endpoint returns.
type recordingMeta struct {
	ID      string `json:"id"`
	User    string `json:"user"`
	Project string `json:"project"`
	// Kind says what the pod belonged to: "app" (a replica; App is set) or
	// "task" (a run; Task is set). Sidecars from before Task shells have no
	// kind and are App shells.
	Kind      string `json:"kind"`
	App       string `json:"app,omitempty"`
	Task      string `json:"task,omitempty"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Shell     string `json:"shell"`
	// DebugContainer is the toolbox container a debug shell ran in.
	DebugContainer string     `json:"debugContainer,omitempty"`
	IP             string     `json:"ip"`
	Started        time.Time  `json:"started"`
	Ended          *time.Time `json:"ended,omitempty"`
	Duration       float64    `json:"durationSeconds"`
	Reason         string     `json:"reason,omitempty"` // why the session ended
	ExitCode       *int       `json:"exitCode,omitempty"`
	Bytes          int64      `json:"bytes"`
	// Input is false: keystrokes are not recorded (see above).
	Input bool `json:"inputRecorded"`
	// Live is set by the list endpoint for sessions still running. A
	// recording that has not ended and is not live was cut short (the
	// console stopped during the session).
	Live bool `json:"live,omitempty"`
}

// castHeader is the first line of an asciicast v2 file. Field order follows
// the specification.
type castHeader struct {
	Version   int               `json:"version"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Timestamp int64             `json:"timestamp"`
	Title     string            `json:"title,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
}

type recording struct {
	r     *recorder
	meta  recordingMeta
	f     *os.File
	w     *bufio.Writer
	start time.Time

	mu    sync.Mutex
	size  int64
	carry []byte // the start of a UTF-8 character split across reads
	full  bool
	done  bool
}

func (rc *recorder) path(id, ext string) string { return filepath.Join(rc.dir, id+ext) }

// start creates the recording file and writes its header. A session must not
// start when this fails.
func (rc *recorder) start(meta recordingMeta, width, height int) (*recording, error) {
	if err := os.MkdirAll(rc.dir, 0o700); err != nil {
		return nil, err
	}
	meta.Started = meta.Started.UTC()
	rc.prune(meta.Started)
	var b [4]byte
	_, _ = rand.Read(b[:])
	meta.ID = meta.Started.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
	f, err := os.OpenFile(rc.path(meta.ID, ".cast"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	rec := &recording{r: rc, meta: meta, f: f, w: bufio.NewWriterSize(f, 32<<10), start: meta.Started}
	header, _ := json.Marshal(castHeader{
		Version: 2, Width: width, Height: height, Timestamp: meta.Started.Unix(),
		Title: fmt.Sprintf("%s in %s/%s (%s)", meta.User, meta.Project, meta.Pod, meta.Container),
		Env:   map[string]string{"TERM": "xterm-256color", "SHELL": meta.Shell},
	})
	n, err := rec.w.Write(append(header, '\n'))
	rec.size += int64(n)
	if err == nil {
		err = rec.w.Flush()
	}
	if err == nil {
		err = rc.writeMeta(&rec.meta)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	rc.setLive(meta.ID, true)
	return rec, nil
}

func (rc *recorder) setLive(id string, live bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.live == nil {
		rc.live = map[string]bool{}
	}
	if live {
		rc.live[id] = true
	} else {
		delete(rc.live, id)
	}
}

func (rc *recorder) isLive(id string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.live[id]
}

// output records terminal output. It returns errRecordingFull once the
// recording reached its size limit; the session must then end.
func (rec *recording) output(p []byte, at time.Time) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	data := append(rec.carry, p...)
	complete, rest := splitUTF8(data)
	rec.carry = append([]byte(nil), rest...)
	if len(complete) == 0 {
		return nil
	}
	return rec.event(at, "o", string(complete))
}

func (rec *recording) resize(cols, rows int, at time.Time) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.event(at, "r", fmt.Sprintf("%dx%d", cols, rows))
}

// event writes one [time, code, data] line. Callers hold mu.
func (rec *recording) event(at time.Time, code, data string) error {
	if rec.done {
		return os.ErrClosed
	}
	if rec.full {
		return errRecordingFull
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Replace invalid UTF-8 ourselves: Go versions differ in how the JSON
	// encoder writes it (an escaped \ufffd or the character itself).
	_ = enc.Encode(validUTF8(data))
	line := fmt.Sprintf("[%.6f, %q, %s]\n", max(at.Sub(rec.start).Seconds(), 0), code, bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	if rec.size+int64(len(line)) > rec.r.maxBytes {
		rec.full = true
		marker := fmt.Sprintf("[%.6f, \"m\", \"recording limit of %d MiB reached; session ended\"]\n", max(at.Sub(rec.start).Seconds(), 0), rec.r.maxBytes>>20)
		_, _ = rec.w.WriteString(marker)
		return errRecordingFull
	}
	n, err := rec.w.WriteString(line)
	rec.size += int64(n)
	return err
}

// flush writes buffered events to disk; the session calls it every second.
func (rec *recording) flush() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.done {
		return nil
	}
	return rec.w.Flush()
}

// finish closes the file and completes the sidecar.
func (rec *recording) finish(at time.Time, reason string, exitCode *int) error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.done {
		return nil
	}
	if len(rec.carry) > 0 && !rec.full {
		_ = rec.event(at, "o", string(rec.carry))
	}
	rec.done = true
	err := rec.w.Flush()
	if cerr := rec.f.Close(); err == nil {
		err = cerr
	}
	end := at.UTC()
	rec.meta.Ended, rec.meta.Reason, rec.meta.ExitCode = &end, reason, exitCode
	rec.meta.Duration = at.Sub(rec.start).Seconds()
	rec.meta.Bytes = rec.size
	if merr := rec.r.writeMeta(&rec.meta); err == nil {
		err = merr
	}
	rec.r.setLive(rec.meta.ID, false)
	return err
}

// writeMeta replaces the sidecar atomically.
func (rc *recorder) writeMeta(m *recordingMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := rc.path(m.ID, ".json.tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, rc.path(m.ID, ".json"))
}

// prune deletes recordings older than the retention, at most once an hour.
func (rc *recorder) prune(now time.Time) {
	rc.mu.Lock()
	if now.Sub(rc.lastPrune) < time.Hour {
		rc.mu.Unlock()
		return
	}
	rc.lastPrune = now
	rc.mu.Unlock()
	entries, err := os.ReadDir(rc.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".cast")
		if !ok || !recordingIDRE.MatchString(id) {
			continue
		}
		info, err := e.Info()
		if err != nil || now.Sub(info.ModTime()) < rc.retention {
			continue
		}
		for _, ext := range []string{".cast", ".json"} {
			if err := os.Remove(rc.path(id, ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
				rc.log.Error("could not delete an expired recording", "id", id, "err", err)
			}
		}
		rc.log.Info("expired shell recording deleted", "id", id)
	}
}

// recordingFilter narrows the list; empty fields match everything.
type recordingFilter struct {
	User, Project, App, Task string
}

func (f recordingFilter) match(m *recordingMeta) bool {
	return (f.User == "" || strings.EqualFold(m.User, f.User)) &&
		(f.Project == "" || m.Project == f.Project) &&
		(f.App == "" || m.App == f.App) &&
		(f.Task == "" || m.Task == f.Task)
}

// list returns the newest recordings that match f, at most limit of them.
func (rc *recorder) list(limit int, f recordingFilter) ([]recordingMeta, error) {
	entries, err := os.ReadDir(rc.dir)
	if errors.Is(err, os.ErrNotExist) {
		return []recordingMeta{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []recordingMeta{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !recordingIDRE.MatchString(id) {
			continue
		}
		data, err := os.ReadFile(rc.path(id, ".json"))
		if err != nil {
			continue
		}
		var m recordingMeta
		if json.Unmarshal(data, &m) != nil || m.ID != id {
			continue
		}
		if m.Kind == "" {
			m.Kind = "app"
		}
		if !f.match(&m) {
			continue
		}
		if m.Ended == nil {
			// Running, or cut short: the sidecar has no size or length yet.
			m.Live = rc.isLive(id)
			if info, err := os.Stat(rc.path(id, ".cast")); err == nil {
				m.Bytes = info.Size()
				m.Duration = max(info.ModTime().Sub(m.Started).Seconds(), 0)
			}
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(x, y recordingMeta) int { return y.Started.Compare(x.Started) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// splitUTF8 splits b before an incomplete UTF-8 sequence at its end, which
// the next read completes.
// validUTF8 replaces every invalid byte with U+FFFD, one per byte, so a
// recording shows how much garbage the terminal received.
func validUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError)
		} else {
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func splitUTF8(b []byte) (complete, rest []byte) {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if b[i] < utf8.RuneSelf {
			break
		}
		if utf8.RuneStart(b[i]) {
			if !utf8.FullRune(b[i:]) {
				return b[:i], b[i:]
			}
			break
		}
	}
	return b, nil
}

// ---- endpoints (owners and admins) ---------------------------------------------

func (p *podsAPI) recordingList(w http.ResponseWriter, r *http.Request) {
	if p.rec == nil {
		writeJSON(w, http.StatusOK, []recordingMeta{})
		return
	}
	q := r.URL.Query()
	list, err := p.rec.list(500, recordingFilter{User: q.Get("user"), Project: q.Get("project"), App: q.Get("app"), Task: q.Get("task")})
	if err != nil {
		p.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (p *podsAPI) recordingGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if p.rec == nil || !recordingIDRE.MatchString(id) {
		writeError(w, http.StatusNotFound, "Recording not found.")
		return
	}
	f, err := os.Open(p.rec.path(id, ".cast"))
	if errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusNotFound, "Recording not found.")
		return
	}
	if err != nil {
		p.internalError(w, r, err)
		return
	}
	defer f.Close()
	pr := r.Context().Value(ctxKey{}).(*principal)
	// The console's player fetches the same file with ?play=1; watching a
	// session shows its output as much as downloading it, so both are
	// audited, under their own names.
	action := "recording.download"
	if r.URL.Query().Get("play") == "1" {
		action = "recording.play"
	}
	p.audit(r, pr.user.Email, action, id, "")
	w.Header().Set("Content-Type", "application/x-asciicast")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="kwerft-shell-%s.cast"`, id))
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, "", time.Time{}, f)
}
