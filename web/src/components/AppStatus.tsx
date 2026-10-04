import type { Phase } from "../workloads";
import { words } from "../workloads";

const look: Record<Phase, { cls: string; label: string }> = {
  running: { cls: "ok", label: "Running" },
  deploying: { cls: "info", label: "Deploying" },
  stopped: { cls: "mute", label: "Stopped" },
  pending: { cls: "info", label: "Pending" },
  failed: { cls: "bad", label: "Failed" },
};

// The status pill for an app. Failures show their reason ("Hostname in use").
export function AppStatus({ phase, reason, message }: { phase: Phase; reason?: string; message?: string }) {
  const { cls, label } = look[phase] ?? look.pending;
  let text = label;
  if (phase === "failed" && reason) text = words(reason);
  if (phase === "pending" && reason === "AwaitingBuild") text = "Awaiting build";
  return <span className={`pill ${cls}`} title={message}>{text}</span>;
}
