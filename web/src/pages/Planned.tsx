// Placeholder for sections that land in later roadmap phases (docs/plan.md).
export function Planned({ title, phase, summary }: { title: string; phase: string; summary: string }) {
  return (
    <section className="view">
      <div className="ph"><div><h1>{title}</h1></div></div>
      <div className="empty">
        <h2>Planned for {phase}</h2>
        <p>{summary}</p>
      </div>
    </section>
  );
}
