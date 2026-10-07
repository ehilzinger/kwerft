// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Figures for medium-drain-2026-10-06.md (the preStop sleep post).
//   node drain-figures.mjs && node render.mjs drain-
// Measured numbers are only the ones the post quotes.
import { figure, C, TINT, esc } from './figures-lib.mjs';

const bar = (g, x, y, w, h, color, { dash = false, label, labelColor = C.ink, weight = 600, anchor = 'start' } = {}) => {
  g.add(`<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="6" fill="${TINT[color]}" stroke="${color}" stroke-width="1.25"${dash ? ' stroke-dasharray="5 4"' : ''}/>`);
  if (label) {
    const tx = anchor === 'middle' ? x + w / 2 : anchor === 'end' ? x + w - 10 : x + 10;
    g.text(tx, y + h / 2 + 4.5, label, { size: 12, weight, color: labelColor, anchor });
  }
};
const vline = (g, x, y1, y2, color, dash = false, width = 1.5) =>
  g.add(`<line x1="${x}" x2="${x}" y1="${y1}" y2="${y2}" stroke="${color}" stroke-width="${width}"${dash ? ' stroke-dasharray="4 4"' : ''}/>`);

// 1. One pod's termination, before and after the drain.
figure('drain-fig1-termination-timeline', 445, g => {
  g.title('Why the old pod got requests after it was told to stop',
    'One restart, from the moment the new pod turns Ready; seconds on the axis');
  const x0 = 150, x1 = 700, t0 = -2, t1 = 7;
  const X = t => x0 + (t - t0) * (x1 - x0) / (t1 - t0);
  const ay = 100;
  g.add(`<line x1="${x0}" x2="${x1}" y1="${ay}" y2="${ay}" stroke="${C.axis}"/>`);
  [[0, 'new pod Ready, old pod deleted', 'end'], [1, '~1 s', 'middle'], [5, '5 s', 'middle']].forEach(([t, label, anchor]) => {
    vline(g, X(t), ay - 4, ay + 4, C.axis, false, 1);
    g.text(anchor === 'end' ? X(t) + 4 : X(t), ay - 10, label, { size: 11.5, anchor, color: C.muted });
  });
  const rowH = 30;
  const rowLabel = (y, t) => g.text(22, y + rowH / 2 + 4.5, t, { size: 12.5, color: C.ink2 });
  const lane = (y, head) => g.text(22, y, head, { size: 14, weight: 700, color: C.ink });

  // Before
  let y = 140;
  lane(y, 'Before: no preStop hook');
  let r1 = y + 14, r2 = y + 54;
  rowLabel(r1, 'old pod');
  bar(g, X(-2), r1, X(0) - X(-2), rowH, C.s3, { label: 'serving' });
  bar(g, X(0), r1, X(7) - X(0), rowH, C.axis, { dash: true, label: 'SIGTERM at once, the app exits', labelColor: C.ink2, weight: 400 });
  rowLabel(r2, 'requests to it');
  bar(g, X(-2), r2, X(0) - X(-2), rowH, C.s1, { label: 'answered' });
  bar(g, X(0), r2, X(1) - X(0), rowH, C.s2, { label: '503', anchor: 'middle' });
  g.text(X(1) + 10, r2 + rowH / 2 + 4.5, 'Cilium and keep-alive connections still send here', { size: 12, color: C.ink2 });

  // After
  y = 270;
  lane(y, 'After: a 5 s preStop sleep');
  r1 = y + 14; r2 = y + 54;
  rowLabel(r1, 'old pod');
  bar(g, X(-2), r1, X(0) - X(-2), rowH, C.s3, { label: 'serving' });
  bar(g, X(0), r1, X(5) - X(0), rowH, C.s3, { label: 'preStop sleep: still answers' });
  bar(g, X(5), r1, X(7) - X(5), rowH, C.axis, { dash: true, label: 'SIGTERM, exits', labelColor: C.ink2, weight: 400 });
  rowLabel(r2, 'requests to it');
  bar(g, X(-2), r2, X(1) - X(-2), rowH, C.s1, { label: 'answered' });
  g.text(X(1) + 10, r2 + rowH / 2 + 4.5, 'callers have caught up: nothing arrives', { size: 12, color: C.ink2 });

  // The EndpointSlice change happens at once, in both cases.
  vline(g, X(0), ay + 8, 378, C.ink2, true, 1.25);
  g.text(X(0) + 8, 392, 'At this moment the old pod is ready: false in its EndpointSlice, in both cases.', { size: 12, color: C.ink2 });
  g.source('Measured with 8 parallel loops through edge: 120 of 667 requests answered 503, all within ~1 s of the new pod turning Ready.');
});

// 2. The tally.
figure('drain-fig2-tally', 320, g => {
  g.title('Requests that answered 503 while brouter restarted',
    '8 loops in parallel through edge (Caddy), before and after the drain');
  const x0 = 250, x1 = 700, bh = 34;
  const row = (y, head, sub, failed, total, note) => {
    g.text(22, y + 14, head, { size: 14, weight: 700, color: C.ink });
    g.text(22, y + 32, sub, { size: 12 });
    const wf = (x1 - x0) * failed / total;
    if (wf > 0) bar(g, x0, y, wf, bh, C.s2);
    bar(g, x0 + wf, y, x1 - x0 - wf, bh, C.s3);
    g.text(x0, y + bh + 20, note, { size: 12.5, weight: 600, color: C.ink });
  };
  row(112, 'Before the drain', 'no preStop hook', 120, 667, '120 of 667 answered 503');
  row(204, 'Default 5 s drain', 'three restarts', 0, 1881, '0 of 1,881');
  // legend
  const ly = 92;
  g.add(`<rect x="${x0}" y="${ly - 10}" width="12" height="12" rx="3" fill="${TINT[C.s2]}" stroke="${C.s2}"/>`);
  g.text(x0 + 18, ly, '503', { size: 12 });
  g.add(`<rect x="${x0 + 60}" y="${ly - 10}" width="12" height="12" rx="3" fill="${TINT[C.s3]}" stroke="${C.s3}"/>`);
  g.text(x0 + 78, ly, 'answered', { size: 12 });
  g.source('Bars are each run’s share of requests. Before the parallel test, one restart had dropped 1 of 671.');
});

// 3. The grace period budget.
figure('drain-fig3-grace-period', 330, g => {
  g.title('The grace period starts when the preStop hook starts',
    'Seconds from the pod being deleted to SIGKILL, for an App with the default 5 s drain');
  const x0 = 210, x1 = 640, T = 35;
  const X = t => x0 + t * (x1 - x0) / T;
  const bh = 36;
  const ticks = (y, marks) => marks.forEach(([t, label]) => {
    vline(g, X(t), y - 6, y + bh + 6, C.ink2, false, 1.25);
    g.text(X(t), y + bh + 22, label, { size: 11.5, anchor: 'middle', color: C.ink2 });
  });
  // Kwerft
  let y = 100;
  g.text(22, y + 14, 'Grace = drain + 30', { size: 14, weight: 700, color: C.ink });
  g.text(22, y + 32, '35 s, what Kwerft renders', { size: 12 });
  bar(g, X(0), y, X(5) - X(0), bh, C.s3, { label: 'sleep', anchor: 'middle' });
  bar(g, X(5), y, X(35) - X(5), bh, C.s1, { label: '30 s to finish in-flight requests and exit' });
  ticks(y, [[0, '0'], [5, 'SIGTERM, 5 s'], [35, 'SIGKILL, 35 s']]);
  // Default grace
  y = 200;
  g.text(22, y + 14, 'Grace left at 30', { size: 14, weight: 700, color: C.ink });
  g.text(22, y + 32, 'the trap', { size: 12 });
  bar(g, X(0), y, X(5) - X(0), bh, C.s3, { label: 'sleep', anchor: 'middle' });
  bar(g, X(5), y, X(30) - X(5), bh, C.s4, { label: 'only 25 s left for the app' });
  ticks(y, [[0, '0'], [5, 'SIGTERM, 5 s'], [30, 'SIGKILL, 30 s']]);
  g.source('A 30 s drain with the default grace period would leave the app no time at all.');
});
