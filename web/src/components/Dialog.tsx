import { useEffect, useRef, type FormEvent, type ReactNode } from "react";

type Props = {
  title: string;
  onClose: () => void;
  children: ReactNode;
  /** Buttons for the footer; the dialog renders a form, so a submit button submits. */
  actions: ReactNode;
  onSubmit?: (e: FormEvent) => void;
};

// A modal dialog on the native <dialog> element: focus trap, Escape and the
// backdrop come from the browser. Mount it to open it.
export function Dialog({ title, onClose, children, actions, onSubmit }: Props) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => d?.close();
  }, []);
  return (
    <dialog ref={ref} className="dlg" aria-labelledby="dlg-title" onClose={onClose}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          onSubmit?.(e);
        }}
      >
        <h2 id="dlg-title">{title}</h2>
        <div className="dlg-bd">{children}</div>
        <div className="dlg-ft">{actions}</div>
      </form>
    </dialog>
  );
}
