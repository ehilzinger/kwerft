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
  // Set while the dialog is closed by unmounting rather than by the user. The
  // close event arrives later (it is queued), and in development StrictMode
  // mounts twice, so that event must not close the re-opened dialog.
  const unmounting = useRef(false);
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => {
      if (d?.open) {
        unmounting.current = true;
        d.close();
      }
    };
  }, []);
  return (
    <dialog ref={ref} className="dlg" aria-labelledby="dlg-title"
      onClose={() => {
        if (unmounting.current) unmounting.current = false;
        else onClose();
      }}>
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
