import { useEffect, useRef, useState } from "react";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { shellURL, type ShellKind } from "../pods";
import "../styles/pods.css";

// A shell in a container: xterm.js (bundled, so the CSP stays 'self') on a
// WebSocket to the console, which runs exec as the signed-in user, records
// the session and ends it after a while without input. See api_shell.go for
// the protocol: binary frames carry the terminal, text frames JSON events.

export type ShellTarget = { project: string; app: string; pod: string; containers: string[] };

type Started = { pod: string; container: string; shell: string; recording: string; idleSeconds: number; maxSeconds: number };
type Status =
  | { s: "connecting" }
  | { s: "open"; info: Started }
  | { s: "closed"; message: string; bad?: boolean };

const SHELLS: { id: ShellKind; label: string }[] = [
  { id: "auto", label: "bash, else sh" },
  { id: "bash", label: "bash" },
  { id: "sh", label: "sh" },
];

export function ShellDialog({ target, onClose }: { target: ShellTarget; onClose: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [shell, setShell] = useState<ShellKind>("auto");
  const [container, setContainer] = useState(target.containers[0] ?? "");
  // The session runs with what was picked when it (re)connected.
  const [session, setSession] = useState({ n: 0, shell: "auto" as ShellKind, container: target.containers[0] ?? "" });
  const [status, setStatus] = useState<Status>({ s: "connecting" });

  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => d?.close();
  }, []);

  const reconnect = () => setSession((x) => ({ n: x.n + 1, shell, container }));
  const changed = shell !== session.shell || container !== session.container;

  return (
    <dialog ref={ref} className="shell-dlg" aria-label={`Shell in ${target.pod}`} onClose={onClose}
      // Escape belongs to the terminal (vim, less); the Close button closes.
      onCancel={(e) => e.preventDefault()}>
      <div className="shell-head">
        <b className="mono">{target.pod}</b>
        {target.containers.length > 1 && (
          <select className="input" aria-label="Container" value={container} onChange={(e) => setContainer(e.target.value)}>
            {target.containers.map((c) => <option key={c} value={c}>{c}</option>)}
          </select>
        )}
        <label className="shell-pick">Shell
          <select className="input" value={shell} onChange={(e) => setShell(e.target.value as ShellKind)}>
            {SHELLS.map((s) => <option key={s.id} value={s.id}>{s.label}</option>)}
          </select>
        </label>
        <button type="button" className={changed || status.s === "closed" ? "btn sm pri" : "btn sm"} onClick={reconnect}>
          {status.s === "closed" ? "Reconnect" : changed ? "Switch" : "Restart"}
        </button>
        <button type="button" className="btn sm push" onClick={() => ref.current?.close()}>Close</button>
      </div>
      <Term key={session.n} target={target} shell={session.shell} container={session.container} onStatus={setStatus} />
      <div className="shell-foot" role="status">
        <StatusLine status={status} />
      </div>
    </dialog>
  );
}

function StatusLine({ status }: { status: Status }) {
  switch (status.s) {
    case "connecting":
      return <span>Connecting…</span>;
    case "open": {
      const i = status.info;
      return (
        <>
          <span className="pill info nodot">Recorded</span>
          <span>{i.container} · {i.shell === "auto" ? "bash or sh" : i.shell}</span>
          <span className="dim">Closes after {minutes(i.idleSeconds)} without input, ends after {minutes(i.maxSeconds)}. Keystrokes are not recorded; what the terminal shows is.</span>
        </>
      );
    }
    case "closed":
      return <span className={status.bad ? "err" : undefined}>{status.message}</span>;
  }
}

const minutes = (s: number) => (s % 60 === 0 && s >= 60 ? `${s / 60} min` : `${s} s`);

const endings: Record<string, string> = {
  exited: "The shell exited.",
  idle: "Closed after a while without input.",
  maxDuration: "The session reached its maximum length.",
  signedOut: "Your console session ended.",
  disconnected: "Disconnected.",
  recordingFull: "The session reached the recording size limit.",
  error: "The shell could not run.",
};

function Term({ target, shell, container, onStatus }: { target: ShellTarget; shell: ShellKind; container: string; onStatus: (s: Status) => void }) {
  const el = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const host = el.current;
    if (!host) return;
    const css = getComputedStyle(document.documentElement);
    const v = (name: string, fallback: string) => css.getPropertyValue(name).trim() || fallback;
    const term = new XTerm({
      fontFamily: v("--mono", "monospace"),
      fontSize: 13,
      lineHeight: 1.2,
      cursorBlink: true,
      scrollback: 5000,
      theme: {
        background: v("--term-bg", "#0E131B"),
        foreground: v("--term-ink", "#D3DAE6"),
        cursor: v("--term-acc", "#93B2FF"),
        selectionBackground: "rgba(147, 178, 255, 0.3)",
      },
    });
    const fit = new FitAddon();
    term.loadAddon(fit);
    term.open(host);
    fit.fit();

    let ws: WebSocket | undefined;
    let ended = false;
    let disposed = false;
    const encoder = new TextEncoder();
    const send = (data: string | Uint8Array<ArrayBuffer>) => {
      if (ws?.readyState === WebSocket.OPEN) ws.send(data);
    };
    const subs = [
      term.onData((d) => send(encoder.encode(d))),
      term.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0)))),
      term.onResize(({ cols, rows }) => send(JSON.stringify({ type: "resize", cols, rows }))),
    ];
    const ro = new ResizeObserver(() => {
      try {
        fit.fit();
      } catch {
        /* hidden while closing */
      }
    });
    ro.observe(host);

    // Wait for the monospace font, so xterm measures the right cell size.
    void document.fonts.load(`13px ${v("--mono", "monospace")}`).finally(() => {
      if (disposed) return;
      fit.fit();
      onStatus({ s: "connecting" });
      ws = new WebSocket(shellURL(target.project, target.app, target.pod, { shell, container, cols: term.cols, rows: term.rows }));
      ws.binaryType = "arraybuffer";
      ws.onmessage = (ev) => {
        if (typeof ev.data !== "string") {
          term.write(new Uint8Array(ev.data as ArrayBuffer));
          return;
        }
        const msg = JSON.parse(ev.data) as { type: string; message?: string; reason?: string; code?: number } & Started;
        if (msg.type === "started") {
          onStatus({ s: "open", info: msg });
          term.focus();
        } else if (msg.type === "error") {
          ended = true;
          onStatus({ s: "closed", message: msg.message ?? "The shell could not start.", bad: true });
        } else if (msg.type === "exit") {
          ended = true;
          const text = msg.message || endings[msg.reason ?? ""] || "The session ended.";
          term.write(`\r\n\x1b[2m[${text}]\x1b[0m\r\n`);
          onStatus({ s: "closed", message: text + (msg.code !== undefined && msg.reason === "exited" && !msg.message ? ` Exit code ${msg.code}.` : ""), bad: msg.reason === "error" });
        }
      };
      ws.onclose = () => {
        if (!ended && !disposed) {
          // A refused handshake (signed out, other origin, too many shells)
          // looks the same as a network drop to the browser.
          onStatus({ s: "closed", message: "The connection closed. Reconnect, or reload the page if you were signed out.", bad: true });
        }
      };
    });

    return () => {
      disposed = true;
      ro.disconnect();
      subs.forEach((s) => s.dispose());
      ws?.close();
      term.dispose();
    };
  }, [target.project, target.app, target.pod, shell, container, onStatus]);

  return <div className="shell-term" ref={el} />;
}
