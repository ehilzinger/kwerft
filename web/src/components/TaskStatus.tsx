// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import type { TaskPhase } from "../jobs";
import { words } from "../workloads";

const look: Record<TaskPhase, { cls: string; label: string }> = {
  pending: { cls: "info", label: "Pending" },
  running: { cls: "info", label: "Running" },
  succeeded: { cls: "ok", label: "Succeeded" },
  failed: { cls: "bad", label: "Failed" },
};

// The status pill for a run. A cancelled run is failed but says so quietly;
// a pending one that waits for something says what ("Volume not found").
export function TaskStatus({ phase, reason, message, muted }: { phase: TaskPhase; reason?: string; message?: string; muted?: boolean }) {
  const { cls, label } = look[phase] ?? look.pending;
  let text = label;
  let c = cls;
  if (phase === "failed" && reason === "Cancelled") {
    text = "Cancelled";
    c = "mute";
  } else if (phase === "pending" && reason && !["Pending", "Created"].includes(reason)) {
    text = words(reason);
    c = "warn";
  }
  return <span className={`pill ${muted ? "mute" : c}`} title={message}>{text}</span>;
}

/** Task phase from the full Task object, as the server's summary computes it. */
export function phaseOfTask(status?: { phase?: string }): TaskPhase {
  return (status?.phase?.toLowerCase() as TaskPhase | undefined) ?? "pending";
}
