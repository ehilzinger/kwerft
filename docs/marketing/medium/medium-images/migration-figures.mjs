// Diagrams for "Moving a real production stack from Docker Compose onto my
// own Kubernetes platform" (medium-migration-2026-10-06.md). Every label is
// taken from the post; the only measured numbers are the ones it quotes.
//
//   node migration-figures.mjs && node render.mjs migration-
import { figure, C, TINT, W, esc, MONO } from './figures-lib.mjs';

// Chips laid out left to right, wrapping inside maxW. Returns the y below them.
function chips(g, x, y, maxW, items, color, { mono = true } = {}) {
  let cx = x, cy = y;
  for (const t of items) {
    const w = t.length * (mono ? 7.6 : 7.0) + 22;
    if (cx + w > x + maxW) { cx = x; cy += 32; }
    g.add(`<rect x="${cx}" y="${cy}" width="${w}" height="24" rx="12" fill="${TINT[color]}" stroke="${color}" stroke-width="1.25"/>`);
    g.add(`<text x="${cx + w / 2}" y="${cy + 16.5}" font-size="12" font-weight="600" text-anchor="middle" fill="${C.ink}"${mono ? ` font-family='${MONO}'` : ''}>${esc(t)}</text>`);
    cx += w + 8;
  }
  return cy + 24;
}

function box(g, x, y, w, h, { fill = C.surface, stroke = C.axis, dash = false } = {}) {
  g.add(`<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="10" fill="${fill}" stroke="${stroke}" stroke-width="1.5"${dash ? ' stroke-dasharray="5 4"' : ''}/>`);
}

// 1. Before and after.
figure('migration-fig1-before-after', 690, g => {
  g.title('One Compose box became two Kwerft clusters',
    'Where each service and each cron entry went, managed from one console');

  // Before
  g.text(22, 96, 'Before: one server, Docker Compose and the host’s crontab', { size: 13.5, weight: 700, color: C.ink });
  box(g, 22, 108, W - 44, 172, { fill: C.band, stroke: C.grid });
  const bw = 210, bh = 148, by = 120;
  // routing project
  box(g, 34, by, bw, bh);
  g.text(48, by + 24, 'Compose: routing', { size: 13.5, weight: 700, color: C.ink });
  chips(g, 48, by + 38, bw - 26, ['caddy', 'route-api', 'brouter', 'graphhopper'], C.s1);
  // tiles project
  box(g, 34 + bw + 12, by, bw - 40, bh);
  g.text(48 + bw + 12, by + 24, 'Compose: tiles', { size: 13.5, weight: 700, color: C.ink });
  chips(g, 48 + bw + 12, by + 38, bw - 66, ['tileserver-gl'], C.s1);
  g.text(48 + bw + 12, by + 94, ['/opt/tiles', 'mounted :ro'], { size: 12, mono: true });
  // cron
  const cx = 34 + bw + 12 + bw - 40 + 12, cw = W - 34 - cx;
  box(g, cx, by, cw, bh);
  g.text(cx + 14, by + 24, 'crontab and systemd', { size: 13.5, weight: 700, color: C.ink });
  g.text(cx + 14, by + 46, [
    'segment sync, nightly 05:15',
    'stop import, monthly',
    'database dump, daily · backup, weekly',
    'site deploy, a poll every minute',
    'trail and ski builds, monthly',
    'graph rebuild, 7–8 h (systemd)',
  ], { size: 12 });

  // arrow
  g.arrow(W / 2, 288, W / 2, 320);
  g.text(W / 2 + 12, 308, 'run side by side, then the DNS records move', { size: 12, color: C.muted });

  // After
  g.text(22, 346, 'After: two clusters, one console', { size: 13.5, weight: 700, color: C.ink });
  const ay = 358, ah = 292;
  const lw = 380, rx = 22 + lw + 12, rw = W - 22 - rx;
  box(g, 22, ay, lw, ah, { fill: TINT[C.s1], stroke: C.s1 });
  g.text(36, ay + 24, 'Dedicated server (agent cluster)', { size: 13.5, weight: 700, color: C.ink });
  g.text(36, ay + 46, 'Apps', { size: 11.5, weight: 600, color: C.muted });
  let y = chips(g, 36, ay + 54, lw - 28, ['edge', 'route-api', 'brouter', 'graphhopper', 'site-deployer'], C.s1);
  g.text(36, y + 20, 'Schedules', { size: 11.5, weight: 600, color: C.muted });
  y = chips(g, 36, y + 28, lw - 28, ['segments-sync', 'import-osm', 'db-backup', 'backup', 'trails-ski'], C.s3);
  g.text(36, y + 20, 'Tasks, started by hand', { size: 11.5, weight: 600, color: C.muted });
  y = chips(g, 36, y + 28, lw - 28, ['graph build', 'reseed'], C.s4);
  g.text(36, y + 26, 'heavy jobs at night, one shared flock lock', { size: 12, color: C.ink2 });

  box(g, rx, ay, rw, ah, { fill: TINT[C.s3], stroke: C.s3 });
  g.text(rx + 14, ay + 24, 'The console’s Cloud server', { size: 13.5, weight: 700, color: C.ink });
  g.text(rx + 14, ay + 46, 'Apps', { size: 11.5, weight: 600, color: C.muted });
  y = chips(g, rx + 14, ay + 54, rw - 28, ['tiles', 'origin'], C.s1);
  g.text(rx + 14, y + 20, 'Schedules', { size: 11.5, weight: 600, color: C.muted });
  y = chips(g, rx + 14, y + 28, rw - 28, ['trails-ski-install', 'backup'], C.s3);
  g.add(`<rect x="${rx + 14}" y="${y + 20}" width="${rw - 28}" height="40" rx="8" fill="${C.surface}" stroke="${C.ink2}" stroke-width="1.25"/>`);
  g.text(rx + 28, y + 45, 'Cloud Volume, 350 GiB', { size: 12.5, weight: 600, color: C.ink });

  g.source('Routing and its jobs on the dedicated server; the tiles and their own install and backup on the console’s server.');
});

// 2. The cutover sequence, with the rollback path.
figure('migration-fig2-cutover', 474, g => {
  g.title('Run both, compare, then move one record',
    'The order every piece followed, and how each step is undone');
  const steps = [
    { head: 'Run both', lines: ['new copy under a', 'test name, same', 'data and keys'] },
    { head: 'Compare', lines: ['45 of 46, as before', 'pixel- and byte-', 'identical tiles'] },
    { head: 'Switch', lines: ['A and AAAA records', 'to the new cluster'] },
    { head: 'Keep the old', lines: ['old box still up,', 'answering cached', 'addresses'] },
  ];
  const lane = (y, label) => {
    g.text(22, y, label, { size: 13.5, weight: 700, color: C.ink });
  };
  const sx = 22, sw = 152, gap = 23, top = 108, sh = 96;
  lane(top - 12, 'Services');
  steps.forEach((s, i) => {
    const x = sx + i * (sw + gap);
    g.card(x, top, sw, sh, { head: s.head, lines: s.lines, color: i === 2 ? C.s1 : i === 3 ? C.s3 : C.axis, fill: i < 2 ? C.surface : undefined });
    if (i < steps.length - 1) g.arrow(x + sw, top + sh / 2, x + sw + gap - 2, top + sh / 2);
  });
  // rollback path under the services
  const ry = top + sh + 28;
  const x2 = sx + 2 * (sw + gap) + sw / 2, x3 = sx + 3 * (sw + gap) + sw / 2;
  g.add(`<path d="M${x3},${top + sh} L${x3},${ry} L${x2},${ry} L${x2},${top + sh + 4}" fill="none" stroke="${C.s2}" stroke-width="1.5" stroke-dasharray="5 4" marker-end="url(#x)"/>`);
  g.text((x2 + x3) / 2, ry + 18, 'rollback: put the two records back', { size: 12, weight: 600, color: C.s2, anchor: 'middle' });

  // Jobs lane
  const jt = 300;
  lane(jt - 12, 'Jobs that write outside their machine');
  const jobs = [
    { head: 'Suspended', lines: ['Schedule created,', 'not yet running'] },
    { head: 'Rehearse', lines: ['dry modes: a draft', 'deploy, a test dump'] },
    { head: 'Switch', lines: ['unsuspend; old cron', 'file moved aside'] },
    { head: 'One writer', lines: ['exactly one machine', 'runs each job'] },
  ];
  const jh = 80;
  jobs.forEach((s, i) => {
    const x = sx + i * (sw + gap);
    g.card(x, jt, sw, jh, { head: s.head, lines: s.lines, color: i === 2 ? C.s1 : i === 3 ? C.s3 : C.axis, fill: i < 2 ? C.surface : undefined });
    if (i < jobs.length - 1) g.arrow(x + sw, jt + jh / 2, x + sw + gap - 2, jt + jh / 2);
  });
  const jy = jt + jh + 28;
  g.add(`<path d="M${x3},${jt + jh} L${x3},${jy} L${x2},${jy} L${x2},${jt + jh + 4}" fill="none" stroke="${C.s2}" stroke-width="1.5" stroke-dasharray="5 4" marker-end="url(#x)"/>`);
  g.text((x2 + x3) / 2, jy + 18, 'rollback: move the cron file back', { size: 12, weight: 600, color: C.s2, anchor: 'middle' });

  g.source('Routing, each job and the tiles moved this way. Nothing on the old box was deleted.');
});

// 3. What the migration found, and what it became.
figure('migration-fig3-found-became', 630, g => {
  g.title('What the migration found, and what it became',
    'Each gap on the left showed up during the move; green is a change in Kwerft');
  const rows = [
    { found: ['A brouter restart: 120 of 667', 'requests answered 503'], head: 'drainSeconds', lines: ['a preStop sleep, default 5 s;', '0 of 1,881 dropped after'], kwerft: true },
    { found: ['A reseed needs minutes to stop;', 'a Task got 30 s after SIGTERM'], head: 'stopSeconds', lines: ['1 to 3,600 s between SIGTERM', 'and SIGKILL; the reseed asks 600'], kwerft: true },
    { found: ['A deploy key that has', 'to be a file for ssh'], head: 'secret + mode', lines: ['Secrets as files, 0444 by default;', 'mode: 256 (0400) for root + ssh'], kwerft: true },
    { found: ['The edge needs its test', 'name and its real name'], head: 'several hostnames per port', lines: ['one Service port,', 'a route per hostname'], kwerft: true },
    { found: ['An install Task stuck in', 'ContainerCreating: “Resource busy”'], head: 'readOnly in the container', lines: ['claim read-write; FailedMount', 'events in the Ready message'], kwerft: true },
    { found: ['Every label missing', 'from the rendered maps'], head: 'egress: https', lines: ['a manifest fix: the styles fetch', 'fonts from the public tile host'], kwerft: false },
  ];
  const lx = 22, lw = 290, rx = 360, rw = W - 22 - rx, top = 98, rh = 72, gap = 10;
  g.text(lx, top - 4, 'Found by the migration', { size: 12, weight: 600, color: C.muted });
  g.text(rx, top - 4, 'Became', { size: 12, weight: 600, color: C.muted });
  rows.forEach((r, i) => {
    const y = top + 8 + i * (rh + gap);
    g.card(lx, y, lw, rh, { fill: C.band });
    g.text(lx + 14, y + 31, r.found, { size: 12.5, weight: 600, color: C.ink });
    g.card(rx, y, rw, rh, { head: r.head, lines: r.lines, color: r.kwerft ? C.s3 : C.axis, mono: true, headSize: 13 });
    g.arrow(lx + lw, y + rh / 2, rx - 2, y + rh / 2);
  });
  g.source('Drain, stopSeconds, Secrets as files and hostnames: 0.6 release candidates. The read-only fix: 0.6.0-rc.2 and later.');
});
