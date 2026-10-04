import { Link, Outlet, useRouterState } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { Icon, type IconName } from "./Icon";

type NavItem = { to: string; label: string; icon: IconName };

const sections: { title?: string; items: NavItem[] }[] = [
  { items: [
    { to: "/", label: "Overview", icon: "grid" },
    { to: "/apps", label: "Apps", icon: "box" },
    { to: "/monitoring", label: "Monitoring", icon: "pulse" },
  ] },
  { title: "Infrastructure", items: [
    { to: "/clusters", label: "Clusters & nodes", icon: "server" },
    { to: "/network", label: "Network", icon: "net" },
  ] },
  { title: "Administration", items: [
    { to: "/access", label: "Access", icon: "users" },
  ] },
];

const titles: Record<string, string> = Object.fromEntries(
  sections.flatMap((s) => s.items.map((i) => [i.to, i.label])),
);

export function Shell() {
  const path = useRouterState({ select: (s) => s.location.pathname });
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const section = "/" + (path.split("/")[1] ?? "");

  return (
    <div className="shell">
      <aside className="side">
        <Link to="/" className="brand"><Icon name="logo" />Kwerft</Link>
        <nav className="nav" aria-label="Console">
          {sections.map((s, i) => (
            <div key={i} style={{ display: "contents" }}>
              {s.title && <span className="grp">{s.title}</span>}
              {s.items.map((item) => (
                <Link key={item.to} to={item.to} activeOptions={{ exact: item.to === "/" }}>
                  <Icon name={item.icon} />
                  {item.label}
                </Link>
              ))}
            </div>
          ))}
        </nav>
        <div className="side-foot">
          {version.data ? `v${version.data.version} · ${version.data.platform}` : version.isError ? "API unreachable" : "…"}
        </div>
      </aside>
      <div className="main">
        <header className="topbar">
          <div className="crumb"><b>{titles[section] ?? "Kwerft"}</b></div>
        </header>
        <Outlet />
      </div>
    </div>
  );
}
