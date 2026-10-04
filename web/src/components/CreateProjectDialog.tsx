import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { PROJECT_RE, workloads, type Project } from "../workloads";
import { Dialog } from "./Dialog";
import { Field } from "./Field";

// Creates a Project: a namespace with its own quota, Pod Security level and
// default-deny network policy. Owners and admins only (Kubernetes RBAC).
export function CreateProjectDialog({ onClose, onCreated }: { onClose: () => void; onCreated?: (p: Project) => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();

  const create = useMutation({
    mutationFn: () => workloads.createProject({ name: name.trim(), displayName: displayName.trim() || undefined }),
    onSuccess: async (p) => {
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      onCreated?.(p);
      onClose();
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: "The console could not be reached." }),
  });

  function submit() {
    if (!PROJECT_RE.test(name.trim()) || name.trim().length > 63) {
      setError({ field: "name", message: "Use lowercase letters, digits and dashes, starting and ending with a letter or digit." });
      return;
    }
    setError(undefined);
    create.mutate();
  }

  return (
    <Dialog
      title="New project"
      onClose={onClose}
      onSubmit={submit}
      actions={
        <>
          <button type="button" className="btn" onClick={onClose}>Cancel</button>
          <button className="btn pri" disabled={create.isPending}>{create.isPending ? "Creating…" : "Create project"}</button>
        </>
      }
    >
      <p className="dim" style={{ margin: 0 }}>
        A project groups apps that belong together. It gets its own namespace, resource quota and network isolation: apps in other projects cannot reach it unless you allow them.
      </p>
      <Field id="proj-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="storefront"
        autoFocus required autoComplete="off" spellCheck={false} maxLength={63} error={error?.field === "name" ? error.message : undefined}
        hint="Becomes the namespace. Lowercase letters, digits and dashes; cannot be changed later." />
      <Field id="proj-display" label="Display name (optional)" value={displayName} onChange={(e) => setDisplayName(e.target.value)} placeholder="Storefront"
        error={error?.field === "displayName" ? error.message : undefined} />
      {error && error.field !== "name" && error.field !== "displayName" && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}
