// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Diagrams for the self-upgrade post (medium-upgrades-2026-10-06.md).
// Every label is taken from the post; nothing here is measured data.
//
//   node upgrade-figures.mjs && node render.mjs upgrade-
import { figure, C, TINT, W, esc } from './figures-lib.mjs';

// A box with a tinted fill, a bold name and an optional caption, centred.
function box(g, x, y, w, h, name, caption, color, { fill } = {}) {
  g.add(`<rect x="${x}" y="${y}" width="${w}" height="${h}" rx="8" fill="${fill || TINT[color]}" stroke="${color}" stroke-width="1.5"/>`);
  const cy = caption ? y + h / 2 - 4 : y + h / 2 + 5;
  g.text(x + w / 2, cy, name, { size: 13.5, weight: 700, color: C.ink, anchor: 'middle' });
  if (caption) g.text(x + w / 2, cy + 18, caption, { size: 11.5, color: C.ink2, anchor: 'middle' });
}

// ---------- 2. The phases of an Upgrade ----------
figure('upgrade-fig2-states', 514, g => {
  g.title('One Upgrade object, one phase at a time',
    'Who moves each phase, and where a failure ends');
  g.add(`<defs><linearGradient id="split" x1="0" x2="1" y1="0" y2="0">
    <stop offset="0.5" stop-color="${TINT[C.s1]}"/><stop offset="0.5" stop-color="${TINT[C.s4]}"/></linearGradient></defs>`);
  const bw = 96, gap = 19, x0 = 24, y1 = 92, bh = 58;
  const xs = i => x0 + i * (bw + gap);
  const row = [
    ['Queued', 'one at a time', C.s1],
    ['Preflight', 'nothing changes', C.s1],
    ['Backup', 'DB, etcd, Helm', C.s4],
    ['Running', 'install.sh unit', C.s4],
    ['Verifying', 'up to 10 min', C.s4],
    ['Succeeded', 'Kwerft N runs', C.s3],
  ];
  row.forEach(([n, c, col], i) => {
    box(g, xs(i), y1, bw, bh, n, c, col, i === 2 ? { fill: 'url(#split)' } : {});
    if (i < row.length - 1) g.arrow(xs(i) + bw, y1 + bh / 2, xs(i + 1) - 2, y1 + bh / 2);
  });

  // Failures before anything changed.
  const y2 = 236, fx = xs(1), fw = 2 * bw + gap;
  box(g, fx, y2, fw, bh, 'Failed', '“Nothing was changed.”', C.s2);
  g.arrow(xs(1) + bw / 2, y1 + bh, xs(1) + bw / 2, y2 - 2, { color: C.s2, marker: 'x' });
  g.text(xs(1) + bw / 2 + 7, 196, 'a check blocks', { size: 11.5 });
  g.arrow(xs(2) + bw / 2, y1 + bh, xs(2) + bw / 2, y2 - 2, { color: C.s2, marker: 'x' });
  g.text(xs(2) + bw / 2 + 7, 172, 'backup fails', { size: 11.5 });

  // Failures after the installer started.
  const rx = 400, rw = 150;
  box(g, rx, y2, rw, bh, 'RollingBack', 'helm rollback, in reverse', C.s4);
  g.arrow(xs(3) + bw / 2, y1 + bh, rx + 30, y2 - 2, { color: C.s2, marker: 'x' });
  g.text(xs(3) + bw / 2 + 6, 208, 'installer fails', { size: 11.5, anchor: 'end' });
  g.arrow(xs(4) + bw / 2, y1 + bh, rx + rw - 30, y2 - 2, { color: C.s2, marker: 'x' });
  g.text(xs(4) + bw / 2 + 2, 190, 'verification fails', { size: 11.5 });

  const y3 = 352, ow = 150;
  box(g, 316, y3, ow, bh, 'RolledBack', 'N-1 verified again', C.s3);
  box(g, 486, y3, ow, bh, 'Failed', 'manual steps listed', C.s2);
  g.arrow(rx + 50, y2 + bh, 316 + ow / 2, y3 - 2);
  g.arrow(rx + rw - 50, y2 + bh, 486 + ow / 2, y3 - 2, { color: C.s2, marker: 'x' });

  // Legend.
  const ly = 436;
  g.chip(22, ly, 'the console’s controller', C.s1, { w: 178 });
  g.chip(210, ly, 'the runner, on the old image', C.s4, { w: 206 });
  g.text(22, ly + 44, 'Backup is both: the controller copies the database, the runner takes the etcd snapshot and Helm revisions.', { size: 11.5 });
  g.source('A cancel is honoured until the installer starts. Every phase change is a compare-and-swap on the Upgrade’s status.');
});

// ---------- 1. Who outlives whom ----------
figure('upgrade-fig1-outlives', 494, g => {
  g.title('Who outlives whom',
    'What runs during a Kwerft upgrade, from the backup to the verification');
  const x0 = 200, x1 = 698;
  const tB = x0, tS = 266, tR = 310, tN = 390, tK = 520, tE = 592;
  // Phase band.
  const py = 80;
  [[tB, tS, 'Backup'], [tS, tE, 'Running'], [tE, x1, 'Verifying']].forEach(([a, b, t]) => {
    g.add(`<rect x="${a}" y="${py}" width="${b - a - 2}" height="24" rx="5" fill="${C.band}" stroke="${C.axis}"/>`);
    g.text((a + b) / 2, py + 16.5, t, { size: 12, weight: 600, color: C.ink, anchor: 'middle' });
  });
  const lane = (y, label, sub) => {
    g.text(22, y + 12, label, { size: 13.5, weight: 700, color: C.ink });
    g.text(22, y + 30, sub, { size: 11.5 });
  };
  const bar = (a, b, y, color, t, { dash = false } = {}) => {
    g.add(`<rect x="${a}" y="${y}" width="${b - a}" height="24" rx="6" fill="${TINT[color]}" stroke="${color}" stroke-width="1.25"${dash ? ' stroke-dasharray="5 4"' : ''}/>`);
    if (t) g.text(a + 10, y + 16.5, t, { size: 12, weight: 600, color: C.ink });
  };
  const mark = (x, y, color = C.s2) => g.add(`<line x1="${x}" x2="${x}" y1="${y - 5}" y2="${y + 29}" stroke="${color}" stroke-width="2"/>`);

  let y = 128;
  lane(y, 'Upgrade object', 'in the Kubernetes API');
  bar(x0, x1, y, C.s3, 'phase, backups, one step per stage');
  g.text(x0, y + 42, 'every process reads it to resume', { size: 11.5 });

  y += 70;
  lane(y, 'systemd unit', 'kwerft-upgrade-<name>');
  bar(tS, tE, y, C.s3, 'install.sh --progress');
  g.text(tS, y + 42, 'belongs to the host: outlives everything below', { size: 11.5 });

  y += 70;
  lane(y, 'Runner pod', 'old image, host network');
  bar(x0, tR, y, C.s4, '');
  bar(tR + 12, x1, y, C.s4, 'retried pod resumes from the status');
  g.cross(tR + 6, y + 12);
  g.text(x0, y + 42, 'a pod that fails is retried by its Job, up to four times', { size: 11.5 });

  y += 70;
  lane(y, 'Console pod', 'one replica');
  bar(x0, tK - 4, y, C.s1, 'Kwerft N-1');
  bar(tK + 4, x1, y, C.s1, 'Kwerft N');
  mark(tK, y);
  g.text(tK, y + 42, 'replaced in the Kwerft stage', { size: 11.5, anchor: 'middle' });

  y += 70;
  lane(y, 'k3s and Cilium', 'the cluster underneath');
  bar(x0, x1, y, C.axis, '');
  mark(tR, y);
  mark(tN, y);
  g.text(tR, y + 42, 'k3s may restart', { size: 11.5, anchor: 'middle' });
  g.text(tN + 6, y + 16.5, 'Cilium upgraded (Network stage)', { size: 12, weight: 600, color: C.ink });

  g.source('On a failure the runner, still on the old image, rolls Helm back, and the console pod is replaced again, back to N-1.');
});

// ---------- 3. What is rolled back ----------
figure('upgrade-fig3-rollback', 500, g => {
  g.title('What a failed upgrade puts back, and what it leaves',
    'Helm is rolled back by itself; the rest is safe to leave as it is, or unsafe to touch');
  const x0 = 22, cChip = 262, cWhy = 404, top = 84, rh = 54;
  g.text(x0, top, 'What', { size: 12, weight: 600, color: C.muted });
  g.text(cChip, top, 'On failure', { size: 12, weight: 600, color: C.muted });
  g.text(cWhy, top, 'Why', { size: 12, weight: 600, color: C.muted });
  const BACK = ['Rolled back', C.s3], KEPT = ['Left as it is', C.s4], NEVER = ['Never automatic', C.s2];
  const rows = [
    [['Helm releases whose', 'revision changed'], BACK, ['helm rollback, Kwerft first,', 'Cilium last; then N-1 is verified']],
    [['CRDs'], KEPT, ['they may only gain optional fields,', 'checked when a release is cut']],
    [['The SQLite database'], KEPT, ['migrations stay readable by N-1;', 'copied back only for Rollback-Safe: no']],
    [['Host changes', '(sysctls, nftables)'], KEPT, ['idempotent, and N-1 tolerates them']],
    [['Releases new', 'in the target'], KEPT, ['left installed, named in the message']],
    [['k3s'], NEVER, ['cannot be downgraded; an etcd restore', 'resets the cluster. Documented, never run']],
  ];
  rows.forEach(([what, [chip, col], why], i) => {
    const y = top + 14 + i * (rh + 6);
    g.add(`<rect x="${x0}" y="${y}" width="${W - 44}" height="${rh}" rx="8" fill="${i % 2 ? C.surface : C.band}"/>`);
    const ty = n => y + (n === 1 ? 32 : 23);
    g.text(x0 + 14, ty(what.length), what[0], { size: 12.5, color: C.ink, weight: 600 });
    if (what[1]) g.text(x0 + 14, ty(2) + 17.5, what[1], { size: 12.5, color: what[1].startsWith('(') ? C.ink2 : C.ink, weight: what[1].startsWith('(') ? 400 : 600 });
    g.chip(cChip, y + rh / 2 - 12, chip, col, { w: 124 });
    g.text(cWhy, ty(why.length), why, { size: 12, color: C.ink2 });
  });
  g.source('The runner records every Helm revision before the installer starts; that list is the rollback plan.');
});
