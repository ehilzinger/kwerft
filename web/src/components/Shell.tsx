import { Link, Outlet, useNavigate, useRouterState } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { alertKeys, alertsApi } from "../alerts";
import { Icon, type IconName } from "./Icon";
import { UnreachableBanner } from "./ClusterUI";
import "../styles/monitoring.css";

type NavItem = { to: string; label: string; icon: IconName; adminOnly?: boolean };

const sections: { title?: string; items: NavItem[] }[] = [
  { items: [
    { to: "/", label: "Overview", icon: "grid" },
    { to: "/apps", label: "Apps", icon: "box" },
    { to: "/jobs", label: "Jobs", icon: "clock" },
    { to: "/secrets", label: "Secrets", icon: "key" },
    { to: "/monitoring", label: "Monitoring", icon: "pulse" },
  ] },
  { title: "Infrastructure", items: [
    { to: "/clusters", label: "Clusters & nodes", icon: "server" },
    { to: "/network", label: "Network", icon: "net" },
  ] },
  { title: "Administration", items: [
    { to: "/access", label: "Access", icon: "users" },
    { to: "/settings", label: "Settings", icon: "gear", adminOnly: true },
  ] },
];

const titles: Record<string, string> = {
  ...Object.fromEntries(sections.flatMap((s) => s.items.map((i) => [i.to, i.label]))),
  "/account": "Account",
};

export function Shell() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const section = "/" + (path.split("/")[1] ?? "");
  const user = useQuery({ queryKey: ["session"], queryFn: api.session });
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  // The Monitoring badge: firing alerts. Without alerting (yet), no badge.
  const firing = useQuery({ queryKey: alertKeys.alerts("firing"), queryFn: () => alertsApi.alerts("firing"), refetchInterval: 15000, retry: false, enabled: !!user.data && !user.data.mustEnrol });
  const firingCount = firing.data?.length ?? 0;

  async function signOut() {
    try {
      await api.logout();
    } finally {
      queryClient.removeQueries({ queryKey: ["session"] });
      await navigate({ to: "/login", search: { next: "/" } });
    }
  }

  return (
    <div className="shell">
      <aside className="side">
        <Link to="/" className="brand"><Icon name="logo" />Kwerft</Link>
        <nav className="nav" aria-label="Console">
          {sections.map((s, i) => (
            <div key={i} style={{ display: "contents" }}>
              {s.title && <span className="grp">{s.title}</span>}
              {s.items.filter((item) => !item.adminOnly || user.data?.role === "owner" || user.data?.role === "admin").map((item) => (
                <Link key={item.to} to={item.to} activeOptions={{ exact: item.to === "/" }}>
                  <Icon name={item.icon} />
                  {item.label}
                  {item.to === "/monitoring" && firingCount > 0 && (
                    <span className="count" title={`${firingCount} firing ${firingCount === 1 ? "alert" : "alerts"}`}>
                      {firingCount}<span className="sr"> firing {firingCount === 1 ? "alert" : "alerts"}</span>
                    </span>
                  )}
                </Link>
              ))}
            </div>
          ))}
        </nav>
        <div className="side-foot">
          {user.data && (
            <div className="who">
              <Link to="/account" className="who-link" title="Your account: password, two-factor sign-in, sessions">
                <span className="avatar" aria-hidden="true">{initials(user.data.name)}</span>
                <div><b>{user.data.name}</b><small>{user.data.role}</small></div>
              </Link>
              <button className="btn sm" onClick={signOut}>Sign out</button>
            </div>
          )}
          <span className="ver">{version.data ? `v${version.data.version} · ${version.data.platform}` : version.isError ? "API unreachable" : "…"}</span>
        </div>
      </aside>
      <div className="main">
        <header className="topbar">
          <div className="crumb"><b>{titles[section] ?? "Kwerft"}</b></div>
        </header>
        {user.data && !user.data.mustEnrol && <UnreachableBanner />}
        <Outlet />
      </div>
    </div>
  );
}

function initials(name: string) {
  return name.split(/\s+/).filter(Boolean).slice(0, 2).map((p) => p[0]!.toUpperCase()).join("");
}
