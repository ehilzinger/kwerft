import { Icon } from "../components/Icon";

export function Apps() {
  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Apps</h1>
          <p>Deployments and stateful services across your projects.</p>
        </div>
        <button className="btn pri" disabled title="Arrives with the App reconciler in Phase 1">
          <Icon name="plus" />Deploy app
        </button>
      </div>
      <div className="empty">
        <h2>No apps yet</h2>
        <p>Deploy a container image from any registry, or connect a Git repository and Werft builds and deploys every push.</p>
      </div>
    </section>
  );
}
