import { Outlet, createRootRouteWithContext, createRoute, createRouter, redirect } from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import { api, isUnauthorized } from "./api";
import { Shell } from "./components/Shell";
import { Overview } from "./pages/Overview";
import { Apps } from "./pages/Apps";
import { AppDetail } from "./pages/AppDetail";
import { Deploy } from "./pages/Deploy";
import { Planned } from "./pages/Planned";
import { Setup } from "./pages/Setup";
import { Login } from "./pages/Login";

type Context = { queryClient: QueryClient };

const root = createRootRouteWithContext<Context>()({ component: Outlet });

const setupStatus = (qc: QueryClient) => qc.ensureQueryData({ queryKey: ["setup"], queryFn: api.setupStatus });

// /setup only exists until the owner account does.
const setupRoute = createRoute({
  getParentRoute: () => root,
  path: "/setup",
  component: Setup,
  beforeLoad: async ({ context }) => {
    if ((await setupStatus(context.queryClient)).complete) throw redirect({ to: "/" });
  },
});

// ?next is only ever a path on this site, never an absolute URL (open redirect).
const safeNext = (v: unknown) => (typeof v === "string" && v.startsWith("/") && !v.startsWith("//") ? v : "/");

export const loginRoute = createRoute({
  getParentRoute: () => root,
  path: "/login",
  component: Login,
  validateSearch: (s: Record<string, unknown>) => ({ next: safeNext(s.next) }),
  beforeLoad: async ({ context }) => {
    if (!(await setupStatus(context.queryClient)).complete) throw redirect({ to: "/setup" });
  },
});

// Everything else needs a finished setup and a signed-in user.
const authed = createRoute({
  getParentRoute: () => root,
  id: "authed",
  component: Shell,
  beforeLoad: async ({ context, location }) => {
    if (!(await setupStatus(context.queryClient)).complete) throw redirect({ to: "/setup" });
    try {
      await context.queryClient.ensureQueryData({ queryKey: ["session"], queryFn: api.session });
    } catch (e) {
      if (isUnauthorized(e)) throw redirect({ to: "/login", search: { next: location.href } });
      throw e;
    }
  },
});

const planned = (path: string, title: string, phase: string, summary: string) =>
  createRoute({ getParentRoute: () => authed, path, component: () => <Planned title={title} phase={phase} summary={summary} /> });

const routeTree = root.addChildren([
  setupRoute,
  loginRoute,
  authed.addChildren([
    createRoute({ getParentRoute: () => authed, path: "/", component: Overview }),
    createRoute({ getParentRoute: () => authed, path: "/apps", component: Apps }),
    createRoute({
      getParentRoute: () => authed, path: "/apps/new", component: Deploy,
      validateSearch: (s: Record<string, unknown>): { project?: string } => (typeof s.project === "string" ? { project: s.project } : {}),
    }),
    createRoute({ getParentRoute: () => authed, path: "/apps/$project/$name", component: AppDetail }),
    planned("/monitoring", "Monitoring", "Phase 3", "Alerts with one-click fixes, top consumers, and alert rules routed to email, Slack, webhooks or ntfy."),
    planned("/clusters", "Clusters & nodes", "Phase 5", "Add Hetzner Cloud servers through the API or join dedicated servers with one command; manage more clusters through an outbound agent."),
    planned("/network", "Network", "Phase 4", "Traffic rules between apps with observed hit and drop counts, the server firewall with lock-out protection, and domains with automatic TLS."),
    planned("/access", "Access", "Phase 4", "Members and roles mapped to Kubernetes RBAC, SSO, API tokens and an append-only audit log."),
  ]),
]);

export function makeRouter(queryClient: QueryClient) {
  return createRouter({ routeTree, context: { queryClient } });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof makeRouter>;
  }
}
