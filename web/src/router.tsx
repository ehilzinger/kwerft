import { createRootRoute, createRoute, createRouter } from "@tanstack/react-router";
import { Shell } from "./components/Shell";
import { Overview } from "./pages/Overview";
import { Apps } from "./pages/Apps";
import { Planned } from "./pages/Planned";

const root = createRootRoute({ component: Shell });

const planned = (path: string, title: string, phase: string, summary: string) =>
  createRoute({ getParentRoute: () => root, path, component: () => <Planned title={title} phase={phase} summary={summary} /> });

const routeTree = root.addChildren([
  createRoute({ getParentRoute: () => root, path: "/", component: Overview }),
  createRoute({ getParentRoute: () => root, path: "/apps", component: Apps }),
  planned("/monitoring", "Monitoring", "Phase 3", "Alerts with one-click fixes, top consumers, and alert rules routed to email, Slack, webhooks or ntfy."),
  planned("/clusters", "Clusters & nodes", "Phase 5", "Add Hetzner Cloud servers through the API or join dedicated servers with one command; manage more clusters through an outbound agent."),
  planned("/network", "Network", "Phase 4", "Traffic rules between apps with observed hit and drop counts, the server firewall with lock-out protection, and domains with automatic TLS."),
  planned("/access", "Access", "Phase 4", "Members and roles mapped to Kubernetes RBAC, SSO, API tokens and an append-only audit log."),
]);

export const router = createRouter({ routeTree });

declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}
