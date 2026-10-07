// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useMemo, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import "@xterm/xterm/css/xterm.css";
import { podsApi, type Recording } from "../pods";
import "../styles/pods.css";
import "../styles/recordings.css";

// Plays a shell recording (asciicast v2, recording.go) in xterm.js.
//
// Why not asciinema-player: it runs its terminal emulator as WebAssembly
// compiled from bytes at runtime, which needs 'wasm-unsafe-eval' in the
// Content-Security-Policy; the console's script-src is 'self' only. xterm.js
// is already bundled for shells, and replaying output events is little code.
//
// Fetching the file with ?play=1 is audited as recording.play.

export type CastEvent = { t: number; code: string; data: string };
export type Cast = { width: number; height: number; events: CastEvent[] };

/** parseCast reads asciicast v2. A recording still being written may end in half a line; it is skipped. */
export function parseCast(text: string): Cast {
  const lines = text.split("\n");
  let header: { version?: number; width?: number; height?: number };
  try {
    header = JSON.parse(lines[0]) as typeof header;
  } catch {
    throw new Error("This file is not an asciicast recording.");
  }
  if (header.version !== 2) throw new Error(`Unsupported asciicast version ${String(header.version)}.`);
  const events: CastEvent[] = [];
  for (const line of lines.slice(1)) {
    if (!line.trim()) continue;
    try {
      const ev = JSON.parse(line) as unknown;
      if (Array.isArray(ev) && typeof ev[0] === "number" && typeof ev[1] === "string" && typeof ev[2] === "string") {
        events.push({ t: ev[0], code: ev[1], data: ev[2] });
      }
    } catch {
      /* the unfinished last line of a live session */
    }
  }
  return { width: header.width || 80, height: header.height || 24, events };
}

/** timeline maps event times to playback times, shortening pauses longer than idleLimit seconds. */
export function timeline(events: CastEvent[], idleLimit?: number): number[] {
  let prev = 0;
  let cut = 0;
  return events.map((e) => {
    const gap = e.t - prev;
    if (idleLimit !== undefined && gap > idleLimit) cut += gap - idleLimit;
    prev = e.t;
    return e.t - cut;
  });
}

const IDLE_LIMIT = 2; // seconds, like asciinema's --idle-time-limit
const SPEEDS = [0.5, 1, 2, 4, 8];
const TICK = 30; // ms between playback steps

const clock = (s: number) => {
  const t = Math.max(0, Math.floor(s));
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const sec = String(t % 60).padStart(2, "0");
  return h ? `${h}:${String(m).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
};

export function CastPlayerDialog({ rec, onClose }: { rec: Recording; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [state, setState] = useState<{ s: "loading" } | { s: "error"; message: string } | { s: "ready"; cast: Cast }>({ s: "loading" });

  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => d?.close();
  }, []);

  useEffect(() => {
    const ac = new AbortController();
    (async () => {
      const res = await fetch(podsApi.recordingURL(rec.id, { play: true }), { credentials: "same-origin", signal: ac.signal });
      if (!res.ok) {
        let message = `${res.status} ${res.statusText}`;
        try {
          message = ((await res.json()) as { error?: string }).error ?? message;
        } catch {
          /* not JSON */
        }
        throw new Error(message);
      }
      setState({ s: "ready", cast: parseCast(await res.text()) });
    })().catch((e: unknown) => {
      if (!ac.signal.aborted) setState({ s: "error", message: e instanceof Error ? e.message : String(e) });
    });
    return () => ac.abort();
  }, [rec.id]);

  const where = rec.kind === "task" ? `run ${rec.task}` : `app ${rec.app}`;
  return (
    <dialog ref={ref} className="shell-dlg cast-dlg" aria-label={`Recording of ${rec.user} in ${rec.pod}`} onClose={onClose}>
      <div className="shell-head">
        <b className="mono">{rec.pod}</b>
        <span className="dim">{rec.user} · {rec.project} · {where} · {new Date(rec.started).toLocaleString()}</span>
        <a className="btn sm push" href={podsApi.recordingURL(rec.id)} download>Download .cast</a>
        <button type="button" className="btn sm" onClick={() => ref.current?.close()}>Close</button>
      </div>
      {state.s === "ready" ? (
        <Player cast={state.cast} />
      ) : (
        <>
          <div className="cast-term" />
          <div className="shell-foot" role="status">
            {state.s === "loading" ? <span>Loading the recording…</span> : <span className="err">Could not load the recording: {state.message}</span>}
          </div>
        </>
      )}
    </dialog>
  );
}

// Engine replays events into the terminal against a playback clock. Writes
// and resizes go through xterm's write queue in order (a resize runs in the
// callback of the write before it), so a seek's full reset cannot interleave
// with output still queued.
class Engine {
  i = 0;
  pos = 0;
  playing = false;
  speed = 1;
  private timer = 0;
  private last = 0;
  constructor(
    private term: XTerm,
    private cast: Cast,
    private times: number[],
    private onTick: (pos: number, playing: boolean, force?: boolean) => void,
    private onMarker: (text: string) => void,
  ) {}

  get total() {
    return this.times.length ? this.times[this.times.length - 1] : 0;
  }

  setTimes(times: number[]) {
    this.times = times;
    this.pos = this.i > 0 ? times[this.i - 1] : 0;
    this.onTick(this.pos, this.playing, true);
  }

  play() {
    if (this.playing) return;
    if (this.pos >= this.total) this.seek(0);
    this.playing = true;
    this.last = performance.now();
    this.timer = window.setTimeout(this.frame, TICK);
    this.onTick(this.pos, true);
  }

  pause() {
    this.playing = false;
    clearTimeout(this.timer);
    this.onTick(this.pos, false);
  }

  seek(to: number) {
    const { width, height } = this.cast;
    this.term.write("\x1bc", () => this.term.resize(width, height)); // full reset (RIS), in the queue
    this.i = 0;
    this.pos = Math.min(Math.max(to, 0), this.total);
    this.apply();
    this.onTick(this.pos, this.playing, true);
  }

  dispose() {
    this.playing = false;
    clearTimeout(this.timer);
  }

  // A timer, not requestAnimationFrame: playback keeps its pace in a hidden
  // tab (xterm paints when it is shown again), and a long stall (a sleeping
  // laptop) does not skip ahead.
  private frame = () => {
    if (!this.playing) return;
    const now = performance.now();
    this.pos += (Math.min(now - this.last, 1000) / 1000) * this.speed;
    this.last = now;
    this.apply();
    if (this.i >= this.times.length) {
      this.pos = this.total;
      this.playing = false;
      this.onTick(this.pos, false);
      return;
    }
    this.onTick(this.pos, true);
    this.timer = window.setTimeout(this.frame, TICK);
  };

  // apply writes every event due at pos, batching output.
  private apply() {
    const { events } = this.cast;
    let out = "";
    while (this.i < events.length && this.times[this.i] <= this.pos) {
      const e = events[this.i++];
      if (e.code === "o") {
        out += e.data;
      } else if (e.code === "r") {
        const m = /^(\d+)x(\d+)$/.exec(e.data);
        if (m) {
          const [cols, rows] = [Number(m[1]), Number(m[2])];
          this.term.write(out, () => this.term.resize(cols, rows));
          out = "";
        }
      } else if (e.code === "m") {
        this.onMarker(e.data);
      }
    }
    if (out) this.term.write(out);
  }
}

function Player({ cast }: { cast: Cast }) {
  const el = useRef<HTMLDivElement>(null);
  const engine = useRef<Engine | null>(null);
  const [ui, setUi] = useState({ pos: 0, playing: false });
  const [speed, setSpeed] = useState(1);
  const [skipIdle, setSkipIdle] = useState(true);
  const [marker, setMarker] = useState<string>();
  const times = useMemo(() => timeline(cast.events, skipIdle ? IDLE_LIMIT : undefined), [cast, skipIdle]);
  const total = times.length ? times[times.length - 1] : 0;
  const empty = !cast.events.some((e) => e.code === "o");

  useEffect(() => {
    const host = el.current;
    if (!host) return;
    const css = getComputedStyle(document.documentElement);
    const v = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback;
    const term = new XTerm({
      cols: cast.width,
      rows: cast.height,
      fontFamily: v("--mono", "monospace"),
      fontSize: 13,
      lineHeight: 1.2,
      disableStdin: true,
      cursorBlink: false,
      scrollback: 5000,
      theme: {
        background: v("--term-bg", "#0E131B"),
        foreground: v("--term-ink", "#D3DAE6"),
        cursor: v("--term-acc", "#93B2FF"),
        selectionBackground: "rgba(147, 178, 255, 0.3)",
      },
    });
    term.open(host);
    // Throttle React updates to a few per second while playing; the terminal
    // itself advances every tick. Play and pause always update.
    let lastUi = 0;
    let wasPlaying = false;
    const e = new Engine(term, cast, times, (pos, playing, force) => {
      const now = performance.now();
      if (force || playing !== wasPlaying || !playing || now - lastUi > 200) {
        lastUi = now;
        wasPlaying = playing;
        setUi({ pos, playing });
      }
    }, setMarker);
    engine.current = e;
    void document.fonts.load(`13px ${v("--mono", "monospace")}`).finally(() => {
      if (engine.current === e) e.play();
    });
    return () => {
      e.dispose();
      engine.current = null;
      term.dispose();
    };
    // times is applied through setTimes below; the terminal lives as long as the cast.
  }, [cast]);

  useEffect(() => {
    engine.current?.setTimes(times);
  }, [times]);

  useEffect(() => {
    if (engine.current) engine.current.speed = speed;
  }, [speed]);

  const toggle = () => (engine.current?.playing ? engine.current.pause() : engine.current?.play());
  const ended = !ui.playing && total > 0 && ui.pos >= total;

  return (
    <>
      <div className="cast-term" ref={el} />
      <div className="shell-foot cast-controls">
        <button type="button" className="btn sm pri" onClick={toggle} disabled={empty}>
          {ui.playing ? "Pause" : ended ? "Replay" : "Play"}
        </button>
        <input type="range" className="cast-seek" aria-label="Position" min={0} max={total || 0} step={0.1} value={Math.min(ui.pos, total)}
          disabled={empty} onChange={(ev) => engine.current?.seek(Number(ev.target.value))} />
        <span className="mono cast-time">{clock(ui.pos)} / {clock(total)}</span>
        <label className="shell-pick">Speed
          <select className="input" value={speed} onChange={(ev) => setSpeed(Number(ev.target.value))}>
            {SPEEDS.map((s) => <option key={s} value={s}>{s}×</option>)}
          </select>
        </label>
        <label className="shell-pick" title={`Pauses longer than ${IDLE_LIMIT} s play as ${IDLE_LIMIT} s`}>
          <input type="checkbox" checked={skipIdle} onChange={(ev) => setSkipIdle(ev.target.checked)} />Skip pauses
        </label>
        {empty ? <span className="dim">Nothing was shown in this session.</span>
          : marker ? <span className="dim">{marker}</span>
          : <span className="dim">Output only: keystrokes were not recorded.</span>}
      </div>
    </>
  );
}
