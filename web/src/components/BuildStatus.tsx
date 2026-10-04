import { buildLook, type Build } from "../builds";

// The status pill of a build: Queued, Building, Deployed, Superseded, Failed, …
export function BuildStatus({ build }: { build: Build }) {
  const { cls, label } = buildLook(build);
  return <span className={`pill ${cls}`} title={build.statusMessage}>{label}</span>;
}
