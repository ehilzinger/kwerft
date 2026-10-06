// Cover images for the five Kwerft posts: the first image of each post, which
// Medium also uses as the preview in feeds and when the post is shared.
// 720×378 (1.9:1, the usual link-preview shape), rendered at 2x.
//
//   node cover-figures.mjs && node render.mjs cover-
import { figure, C, TINT, esc, MONO } from './figures-lib.mjs';

const H = 378;
const kicker = (g, t) => g.add(`<text x="40" y="54" font-size="13" font-weight="700" letter-spacing="1.6" fill="${C.muted}">${esc(t.toUpperCase())}</text>`);
const head = (g, lines, y = 96, size = 30) => lines.forEach((l, i) =>
  g.add(`<text x="40" y="${y + i * (size + 8)}" font-size="${size}" font-weight="800" fill="${C.ink}">${esc(l)}</text>`));
const bg = (g) => {
  g.add(`<rect width="720" height="${H}" fill="${C.band}"/>`);
  g.add(`<rect x="0" y="${H - 8}" width="720" height="8" fill="${C.s1}"/>`);
};
const box = (g, x, y, w, h, { fill = C.surface, stroke = C.axis, r = 10, dash = false } = {}) =>
  g.add(`<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="${r}" fill="${fill}" stroke="${stroke}" stroke-width="1.5"${dash ? ' stroke-dasharray="5 4"' : ''}/>`);
const mono = (g, x, y, t, { size = 12.5, color = C.ink2, weight = 400, anchor = 'start' } = {}) =>
  g.add(`<text x="${x}" y="${y}" font-size="${size}" font-weight="${weight}" text-anchor="${anchor}" fill="${color}" font-family='${MONO}'>${esc(t)}</text>`);
const txt = (g, x, y, t, { size = 13, color = C.ink2, weight = 400, anchor = 'start' } = {}) =>
  g.add(`<text x="${x}" y="${y}" font-size="${size}" font-weight="${weight}" text-anchor="${anchor}" fill="${color}">${esc(t)}</text>`);

// 1. Drain: the old pod keeps answering through a 5 s sleep; 503s go to zero.
figure('cover-drain', H, g => {
  bg(g);
  kicker(g, 'Kubernetes rollouts');
  head(g, ['Zero dropped requests,', 'one five-second sleep']);
  const x0 = 40, y = 210, w = 640;
  txt(g, x0, y - 12, 'old pod', { size: 12.5, weight: 700, color: C.ink });
  box(g, x0, y, 210, 40, { fill: TINT[C.s3], stroke: C.s3 });
  txt(g, x0 + 14, y + 25, 'serving', { weight: 600, color: C.ink });
  box(g, x0 + 216, y, 250, 40, { fill: TINT[C.s3], stroke: C.s3 });
  mono(g, x0 + 230, y + 25, 'preStop: sleep 5', { weight: 700, color: C.ink });
  box(g, x0 + 472, y, w - 472, 40, { fill: C.surface, stroke: C.axis, dash: true });
  txt(g, x0 + 486, y + 25, 'SIGTERM, exits', { color: C.muted });
  g.add(`<line x1="${x0 + 213}" x2="${x0 + 213}" y1="${y - 26}" y2="${y + 112}" stroke="${C.ink2}" stroke-dasharray="3 4"/>`);
  txt(g, x0 + 220, y - 12, 'new pod Ready', { size: 12, color: C.muted });
  // the tally
  const ty = y + 72;
  txt(g, x0, ty + 26, '503s while it restarts', { size: 13, color: C.ink2 });
  g.add(`<text x="${x0 + 216}" y="${ty + 30}" font-size="30" font-weight="800" fill="${C.s2}">120 of 667</text>`);
  g.arrow(x0 + 400, ty + 19, x0 + 462, ty + 19, { width: 2 });
  g.add(`<text x="${x0 + 472}" y="${ty + 30}" font-size="30" font-weight="800" fill="${C.s3}">0 of 1,881</text>`);
});

// 2. Migration: one Compose box with a crontab becomes two clusters.
figure('cover-migration', H, g => {
  bg(g);
  kicker(g, 'Docker Compose to Kubernetes');
  head(g, ['A real production stack,', 'moved and still running']);
  const y = 180;
  box(g, 40, y, 250, 158);
  mono(g, 58, y + 30, 'docker compose', { weight: 700, color: C.ink, size: 14 });
  ['brouter', 'graphhopper', 'route-api', 'caddy'].forEach((n, i) => mono(g, 58 + (i % 2) * 110, y + 58 + Math.floor(i / 2) * 22, n));
  g.add(`<line x1="58" x2="272" y1="${y + 102}" y2="${y + 102}" stroke="${C.grid}"/>`);
  mono(g, 58, y + 124, '15 5 * * *  sync-segments', { size: 12 });
  mono(g, 58, y + 144, '… 7 more cron lines', { size: 12, color: C.muted });
  g.arrow(300, y + 79, 360, y + 79, { width: 2 });
  const card = (x, yy, h, title, chips, color) => {
    box(g, x, yy, 320, h, { fill: TINT[color], stroke: color });
    txt(g, x + 16, yy + 26, title, { weight: 700, color: C.ink, size: 14 });
    let cx = x + 16;
    chips.forEach((c) => { cx += g.chip(cx, yy + 38, c, color) + 6; });
  };
  card(368, y - 4, 76, 'Dedicated server', ['Apps', 'Schedules', 'Tasks'], C.s1);
  card(368, y + 86, 76, 'Cloud server', ['tiles', '350 GiB Volume'], C.s3);
});

// 3. RBAC: the console asks Kubernetes as you.
figure('cover-rbac', H, g => {
  bg(g);
  kicker(g, 'Kubernetes RBAC in Go');
  head(g, ['Let the API server', 'decide, as each user']);
  const y = 184;
  box(g, 40, y, 420, 150, { fill: '#10141a', stroke: '#10141a' });
  mono(g, 60, y + 32, 'PATCH /apis/kwerft.dev/v1alpha1/…/apps/shop', { color: '#c9d1d9', size: 12 });
  mono(g, 60, y + 64, 'Impersonate-User:', { color: '#7ee2b8', size: 12.5 });
  mono(g, 200, y + 64, 'kwerft:mara@…', { color: '#e6edf3', size: 12.5 });
  mono(g, 60, y + 90, 'Impersonate-Group:', { color: '#7ee2b8', size: 12.5 });
  mono(g, 208, y + 90, 'kwerft:role:developer', { color: '#e6edf3', size: 12.5 });
  mono(g, 60, y + 116, 'Impersonate-Group:', { color: '#7ee2b8', size: 12.5 });
  mono(g, 208, y + 116, 'system:authenticated', { color: '#e6edf3', size: 12.5 });
  g.arrow(470, y + 75, 516, y + 75, { width: 2 });
  box(g, 524, y + 10, 156, 54, { fill: TINT[C.s3], stroke: C.s3 });
  txt(g, 602, y + 43, 'allowed', { weight: 700, color: C.ink, anchor: 'middle', size: 15 });
  box(g, 524, y + 82, 156, 54, { fill: TINT[C.s2], stroke: C.s2 });
  txt(g, 602, y + 115, '403 Forbidden', { weight: 700, color: C.ink, anchor: 'middle', size: 15 });
  txt(g, 602, y + 158, 'RoleBindings decide', { size: 12, color: C.muted, anchor: 'middle' });
});

// 4. Upgrades: the old version watches the new one.
figure('cover-upgrades', H, g => {
  bg(g);
  kicker(g, 'Self-upgrading software');
  head(g, ['The watcher has to be', 'the old version']);
  const y = 186;
  box(g, 40, y, 200, 76, { fill: TINT[C.s4], stroke: C.s4 });
  txt(g, 56, y + 28, 'runner', { weight: 700, color: C.ink, size: 15 });
  mono(g, 56, y + 54, '0.6.0 · old image', { size: 12.5 });
  box(g, 480, y, 200, 76, { fill: TINT[C.s1], stroke: C.s1 });
  txt(g, 496, y + 28, 'console', { weight: 700, color: C.ink, size: 15 });
  mono(g, 496, y + 54, '0.6.1 · new', { size: 12.5 });
  g.arrow(248, y + 38, 472, y + 38, { width: 2, label: 'verifies, rolls back', ly: y + 28 });
  let x = 40;
  ['Backup', 'Running', 'Verifying'].forEach((s) => { x += g.chip(x, y + 104, s, C.axis) + 10; });
  g.arrow(x, y + 116, x + 28, y + 116, { color: C.s2, marker: 'x' });
  x += 36;
  x += g.chip(x, y + 104, 'RolledBack', C.s4) + 10;
  txt(g, x + 4, y + 121, 'k3s: never automatically', { size: 12.5, color: C.muted });
});

// 5. Backups: one recovery key, everything in the bucket encrypted.
figure('cover-backups', H, g => {
  bg(g);
  kicker(g, 'Velero on S3-compatible storage');
  head(g, ['One recovery key,', 'every object encrypted']);
  const y = 196;
  box(g, 40, y + 30, 170, 70, { fill: TINT[C.s4], stroke: C.s4 });
  txt(g, 56, y + 60, 'recovery key', { weight: 700, color: C.ink, size: 15 });
  mono(g, 56, y + 84, 'K7QM-2VX9-…', { size: 12.5 });
  const rows = [
    ['Velero objects', 'SSE-C · HKDF-SHA256', C.s1],
    ['volume data', 'Kopia', C.s3],
    ['etcd snapshots', 'SSE-C · HKDF-SHA256', C.s1],
  ];
  rows.forEach(([what, how, color], i) => {
    const ry = y + i * 46;
    g.arrow(218, y + 65, 300, ry + 19, { width: 1.5 });
    box(g, 306, ry, 374, 38, { fill: TINT[color], stroke: color });
    txt(g, 322, ry + 24, what, { weight: 700, color: C.ink });
    mono(g, 664, ry + 24, how, { size: 12, anchor: 'end' });
  });
});
