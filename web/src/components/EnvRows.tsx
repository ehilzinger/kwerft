import { Icon } from "./Icon";

export type EnvRow = { name: string; value: string };

export const ENV_NAME_RE = /^[-._a-zA-Z][-._a-zA-Z0-9]*$/;

/** Rows with a name or value, trimmed; the index of the first bad name, if any. */
export function envOf(rows: EnvRow[]): { vars: EnvRow[]; bad?: number } {
  const vars: EnvRow[] = [];
  for (const [i, r] of rows.entries()) {
    if (!r.name.trim() && !r.value) continue;
    if (!ENV_NAME_RE.test(r.name.trim())) return { vars, bad: i };
    vars.push({ name: r.name.trim(), value: r.value });
  }
  return { vars };
}

// Key/value rows for environment variables, e.g. the overrides of a run
// (FORCE=1, DRY_RUN=1). errorAt marks a row with a message under it.
export function EnvRows({ rows, onChange, label, errorAt, placeholder = "NAME" }: {
  rows: EnvRow[];
  onChange: (rows: EnvRow[]) => void;
  label: string;
  errorAt?: { index: number; message: string };
  placeholder?: string;
}) {
  const set = (i: number, patch: Partial<EnvRow>) => onChange(rows.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <div className="env-rows" role="group" aria-label={label}>
      {rows.map((r, i) => (
        <div className="env-row" key={i}>
          <input className="input mono" aria-label={`${label} ${i + 1} name`} value={r.name} placeholder={placeholder} spellCheck={false} autoComplete="off"
            aria-invalid={errorAt?.index === i} onChange={(e) => set(i, { name: e.target.value })} />
          <span className="eq" aria-hidden="true">=</span>
          <input className="input mono" aria-label={`${label} ${i + 1} value`} value={r.value} placeholder="value" spellCheck={false} autoComplete="off"
            onChange={(e) => set(i, { value: e.target.value })} />
          <button type="button" className="btn ghost sm danger" aria-label={`Remove ${r.name || "variable"}`} onClick={() => onChange(rows.filter((_, j) => j !== i))}>Remove</button>
          {errorAt?.index === i && <span className="field-error full" role="alert">{errorAt.message}</span>}
        </div>
      ))}
      <button type="button" className="btn sm" onClick={() => onChange([...rows, { name: "", value: "" }])}><Icon name="plus" />Add variable</button>
    </div>
  );
}
