import { useState } from "react";
import { createPortal } from "react-dom";
import { useMutation } from "@tanstack/react-query";
import { ApiError } from "../api";
import { secretsApi } from "../secrets";
import { CopyButton } from "./CopyButton";
import { Dialog } from "./Dialog";
import { Field } from "./Field";

// Reveal one secret value: owners and admins, after their password (or a
// current authenticator code). The server audits it; the value lives only in
// this dialog's state and is gone when it closes.
export function RevealSecret({ project, set, secretKey, className = "btn ghost sm" }: { project: string; set: string; secretKey: string; className?: string }) {
  const [open, setOpen] = useState(false);
  return (
    <>
      <button type="button" className={className} onClick={() => setOpen(true)}
        title="Owners and admins, after their password. Recorded in the audit log.">Reveal</button>
      {/* A portal, since the button may sit in a form; React still bubbles
          the dialog's submit to that form, hence the stop. */}
      {open && createPortal(
        <div onSubmit={(e) => e.stopPropagation()}>
          <RevealDialog project={project} set={set} secretKey={secretKey} onClose={() => setOpen(false)} />
        </div>, document.body)}
    </>
  );
}

function RevealDialog({ project, set, secretKey, onClose }: { project: string; set: string; secretKey: string; onClose: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const reveal = useMutation({
    mutationFn: () => secretsApi.reveal(project, set, secretKey, password),
    onSuccess: () => { setPassword(""); setError(undefined); },
    onError: (e) => setError(e instanceof ApiError ? e.message : "The console could not be reached. Check your connection and try again."),
  });
  const value = reveal.data?.value;
  return (
    <Dialog title={`Reveal ${secretKey}`} onClose={onClose} onSubmit={() => (value === undefined ? reveal.mutate() : onClose())}
      actions={value === undefined ? (
        <>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn pri" disabled={!password || reveal.isPending}>{reveal.isPending ? "Checking…" : "Reveal"}</button>
        </>
      ) : <button className="btn pri">Hide and close</button>}>
      {value === undefined ? (
        <>
          <p style={{ margin: 0 }}>The value of <code>{set}/{secretKey}</code> is shown once, here. Revealing it is recorded in the audit log.</p>
          <Field id="reveal-pw" label="Your password" type="password" autoComplete="current-password" value={password} autoFocus
            onChange={(e) => setPassword(e.target.value)} error={error} hint="Or a current code from your authenticator app." />
        </>
      ) : (
        <div className="field">
          <label htmlFor="reveal-value">{secretKey}</label>
          <input id="reveal-value" className="input mono" readOnly value={value} onFocus={(e) => e.target.select()} spellCheck={false} autoComplete="off" />
          <span><CopyButton text={value} /></span>
        </div>
      )}
    </Dialog>
  );
}
