import type { InputHTMLAttributes } from "react";

type Props = InputHTMLAttributes<HTMLInputElement> & {
  id: string;
  label: string;
  hint?: string;
  error?: string;
};

// A labelled input with an optional hint and an error tied to it for screen readers.
export function Field({ id, label, hint, error, className, ...input }: Props) {
  const describedBy = error ? `${id}-error` : hint ? `${id}-hint` : undefined;
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <input id={id} className={`input ${className ?? ""}`} aria-invalid={!!error} aria-describedby={describedBy} {...input} />
      {error ? (
        <span id={`${id}-error`} className="field-error" role="alert">{error}</span>
      ) : hint ? (
        <span id={`${id}-hint`} className="hint">{hint}</span>
      ) : null}
    </div>
  );
}
