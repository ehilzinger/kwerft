import { Outlet, createRootRouteWithContext, createRoute, createRouter, redirect } from "@tanstack/react-router";
import type { QueryClient } from "@tanstack/react-query";
import { api, isUnauthorized } from "./api";
import { Shell } from "./components/Shell";
import { Overview } from "./pages/Overview";
import { Apps } from "./pages/Apps";
import { AppDetail } from "./pages/AppDetail";
import { Deploy } from "./pages/Deploy";
import { Planned } from "./pages/Planned";
import { MonitoringAlerts } from "./pages/MonitoringAlerts";
import { MonitoringMetrics } from "./pages/MonitoringMetrics";
import { MonitoringLogs, logsSearch } from "./pages/MonitoringLogs";
import { MonitoringRules } from "./pages/MonitoringRules";
import { MonitoringChannels } from "./pages/MonitoringChannels";
import { Setup } from "./pages/Setup";
import { Login } from "./pages/Login";
import { Account } from "./pages/Account";
import { Jobs } from "./pages/Jobs";
import { NewSchedule, ScheduleDetail } from "./pages/ScheduleForm";
import { TaskDetail } from "./pages/TaskDetail";
import { Volumes } from "./pages/Volumes";
import { Network } from "./pages/Network";
import { AccessRecordings, recordingsSearch } from "./pages/AccessRecordings";
import { AccessAudit, AccessMembers, AccessRoles } from "./pages/Access";
import { InviteAccept } from "./pages/InviteAccept";
import { Settings } from "./pages/Settings";

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

// An invite link: public, the token is the credential (see InviteAccept).
export const inviteRoute = createRoute({
  getParentRoute: () => root,
  path: "/invite/$token",
  component: InviteAccept,
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

const projectSearch = (s: Record<string, unknown>): { project?: string } => (typeof s.project === "string" ? { project: s.project } : {});

const planned = (path: string, title: string, phase: string, summary: string) =>
  createRoute({ getParentRoute: () => authed, path, component: () => <Planned title={title} phase={phase} summary={summary} /> });

const routeTree = root.addChildren([
  setupRoute,
  loginRoute,
  inviteRoute,
  authed.addChildren([
    createRoute({ getParentRoute: () => authed, path: "/", component: Overview }),
    createRoute({ getParentRoute: () => authed, path: "/apps", component: Apps }),
    createRoute({ getParentRoute: () => authed, path: "/account", component: Account }),
    createRoute({
      getParentRoute: () => authed, path: "/apps/new", component: Deploy,
      validateSearch: (s: Record<string, unknown>): { project?: string } => (typeof s.project === "string" ? { project: s.project } : {}),
    }),
    // ?build=<name> opens the Builds tab on that build (commit checks link
    // there); ?tab=logs (or another tab) opens that tab — alert
    // notifications link to /apps/<p>/<a>?tab=logs.
    createRoute({
      getParentRoute: () => authed, path: "/apps/$project/$name", component: AppDetail,
      validateSearch: (s: Record<string, unknown>): { build?: string; tab?: string } => ({
        ...(typeof s.build === "string" && s.build ? { build: s.build } : {}),
        ...(typeof s.tab === "string" && s.tab ? { tab: s.tab } : {}),
      }),
    }),
    createRoute({ getParentRoute: () => authed, path: "/apps/volumes", component: Volumes, validateSearch: projectSearch }),
    createRoute({ getParentRoute: () => authed, path: "/jobs", component: Jobs, validateSearch: projectSearch }),
    createRoute({
      getParentRoute: () => authed, path: "/jobs/new", component: NewSchedule,
      validateSearch: (s: Record<string, unknown>): { project?: string; fromApp?: string } => ({
        ...projectSearch(s), ...(typeof s.fromApp === "string" ? { fromApp: s.fromApp } : {}),
      }),
    }),
    createRoute({ getParentRoute: () => authed, path: "/jobs/$project/schedules/$name", component: ScheduleDetail }),
    createRoute({ getParentRoute: () => authed, path: "/jobs/$project/tasks/$name", component: TaskDetail }),
    createRoute({ getParentRoute: () => authed, path: "/network", component: Network }),
    // Access › Shell recordings (owners and admins); /access itself is Members.
    createRoute({ getParentRoute: () => authed, path: "/access/recordings", component: AccessRecordings, validateSearch: recordingsSearch }),
    createRoute({ getParentRoute: () => authed, path: "/settings", component: Settings }),
    // Monitoring tabs (Phase 3): Alerts, Metrics, Logs, Alert rules, Channels.
    createRoute({ getParentRoute: () => authed, path: "/monitoring", component: MonitoringAlerts }),
    createRoute({ getParentRoute: () => authed, path: "/monitoring/metrics", component: MonitoringMetrics }),
    createRoute({ getParentRoute: () => authed, path: "/monitoring/logs", component: MonitoringLogs, validateSearch: logsSearch }),
    createRoute({ getParentRoute: () => authed, path: "/monitoring/rules", component: MonitoringRules }),
    createRoute({ getParentRoute: () => authed, path: "/monitoring/channels", component: MonitoringChannels }),
    planned("/clusters", "Clusters & nodes", "Phase 5", "Add Hetzner Cloud servers through the API or join dedicated servers with one command; manage more clusters through an outbound agent."),
    // Access tabs; /access/recordings is the Recordings tab's own route.
    createRoute({ getParentRoute: () => authed, path: "/access", component: AccessMembers }),
    createRoute({ getParentRoute: () => authed, path: "/access/roles", component: AccessRoles }),
    createRoute({ getParentRoute: () => authed, path: "/access/audit", component: AccessAudit }),
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
