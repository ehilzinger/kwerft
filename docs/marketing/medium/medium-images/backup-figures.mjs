// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Figures for "Encrypting Velero backups on object storage that only offers
// SSE-C" (medium-backups-2026-10-06.md). Every label comes from the post;
// nothing here is measured data.
//
//   node backup-figures.mjs && node render.mjs backup-
import { figure, C, TINT, W, esc, MONO } from './figures-lib.mjs';

const mono = (g, x, y, t, { size = 11.5, color = C.ink2, anchor = 'start' } = {}) =>
  g.add(`<text x="${x}" y="${y}" font-size="${size}" text-anchor="${anchor}" fill="${color}" font-family='${MONO}'>${esc(t)}</text>`);

// 1. What goes where, encrypted by whom.
figure('backup-fig1-what-goes-where', 520, g => {
  g.title('What goes into the bucket, and who encrypts it',
    'One recovery key unlocks all of it; neither key is ever stored in the bucket');
  const lx = 22, lw = 330, rx = 400, rw = 298, rh = 92, gap = 16, top = 84;
  const rows = [
    { head: 'Object tarballs, logs, lists', lines: ['every Secret, the data key, tokens,', 'written by Velero’s AWS plugin'],
      path: '<prefix>/velero/backups/…', enc: 'The storage: SSE-C, AES-256',
      key: ['with the SSE-C key derived', 'from the recovery key'], color: C.s1 },
    { head: 'Volume data', lines: ['file system backup by', 'Velero’s node agent (Kopia)'],
      path: '<prefix>/velero/kopia/<namespace>/', enc: 'Kopia, on the server',
      key: ['with the recovery key itself', '(the repository password)'], color: C.s3 },
    { head: 'etcd snapshots', lines: ['the whole cluster state, uploaded', 'by Kwerft’s agent, not by k3s'],
      path: '<prefix>/etcd/<node>/', enc: 'The storage: SSE-C, AES-256',
      key: ['with the same derived key; k3s’s', 'own S3 upload cannot encrypt'], color: C.s1 },
  ];
  rows.forEach((r, i) => {
    const y = top + i * (rh + gap);
    g.card(lx, y, lw, rh, { head: r.head, lines: r.lines });
    mono(g, lx + 14, y + rh - 12, r.path, { size: 11, color: C.muted });
    g.arrow(lx + lw, y + rh / 2, rx - 2, y + rh / 2);
    g.card(rx, y, rw, rh, { head: r.enc, lines: r.key, color: r.color, headSize: 13.5 });
  });
  const by = top + 3 * (rh + gap) + 2;
  g.add(`<rect x="22" y="${by}" width="${W - 44}" height="60" rx="9" fill="${C.band}"/>`);
  g.text(38, by + 24, 'Never in the bucket: the recovery key and the derived key.', { size: 12.5, weight: 600, color: C.ink });
  g.text(38, by + 44, 'Readable to anyone with bucket access: object names, sizes and metadata.', { size: 12.5 });
  g.source('Kwerft 0.6 release candidates, Velero v1.18.4, velero-plugin-for-aws v1.14.4, Hetzner Object Storage.');
});

// 2. The key tree: one recovery key, two keys.
figure('backup-fig2-key-tree', 580, g => {
  g.title('One recovery key, two keys',
    'The owner keeps one secret; everything else is computed from it');
  const cx = W / 2;
  g.card(cx - 200, 80, 400, 58, { head: 'Recovery key', lines: ['32 random bytes, shown once as 52 base32 characters'], color: C.s4 });
  g.arrow(cx, 138, cx, 176, { label: 'upper case, no dashes or spaces', lx: cx + 10, ly: 162, anchor: 'start' });
  g.card(cx - 200, 178, 400, 40, { head: 'Repository password (52 characters)', headSize: 13.5 });
  // Left branch: Kopia
  const lx = 22, lw = 300, rx = 398, rw = 300, by = 270;
  g.arrow(cx - 80, 218, lx + lw / 2, by - 2, { label: 'as it is', lx: cx - 118, ly: 228, anchor: 'end' });
  g.card(lx, by, lw, 76, { head: 'Kopia repository password', lines: ['Secret velero-repo-credentials', 'encrypts volume data on the server'], color: C.s3 });
  // Right branch: HKDF
  g.arrow(cx + 80, 218, rx + rw / 2, by - 2, { label: 'derived', lx: cx + 118, ly: 228, anchor: 'start' });
  g.card(rx, by, rw, 76, { head: 'HKDF-SHA256, 32 bytes', lines: [], color: C.s1 });
  mono(g, rx + 18, by + 46, 'salt "kwerft.dev/recovery-key"');
  mono(g, rx + 18, by + 63, 'info "kwerft.dev/backups/sse-c/v1"');
  g.arrow(rx + rw / 2, by + 76, rx + rw / 2, by + 104);
  g.card(rx, by + 106, rw, 40, { head: 'SSE-C key (32 bytes, AES-256)', headSize: 13.5, color: C.s1 });
  const uy = by + 168;
  g.arrow(rx + rw / 2, by + 146, rx + rw / 2, uy - 2);
  g.card(rx, uy, rw, 92, { head: 'Computed by', lines: ['the console (Go) and install.sh --restore', '(bash); handed to Velero’s AWS plugin', 'and the etcd snapshot agent as Secrets'], headSize: 13 });
  g.text(lx, by + 112, ['Hetzner sees the SSE-C key with every', 'request. HKDF is one-way: it never', 'learns the repository password.'], { size: 12.5, color: C.ink });
  g.text(lx, by + 186, ['The same vector is tested in Go and in', 'bash; the bash one also against RFC 4231,', 'RFC 5869 and openssl kdf.'], { size: 12.5 });
  g.source('A different function of the same key: the "v1" in the info string leaves room for a rotation.');
});

// 3. The restore path on a fresh server.
figure('backup-fig3-restore-path', 530, g => {
  g.title('A new server needs the bucket and one key',
    'install.sh --config kwerft.yaml --restore latest, stage by stage');
  // What the server needs
  g.card(22, 84, 220, 128, { head: 'What it needs', lines: ['kwerft.yaml: a backups block', '(endpoint, bucket, prefix)', 'the access key file', 'the secret key file', 'the recovery key file'], color: C.s4 });
  g.text(22, 240, ['Nothing from the old server:', 'the bucket is all that is left.'], { size: 12.5 });
  const sx = 280, sw = 418, sh = 46, sg = 12, top = 84;
  const steps = [
    ['The usual stages', 'k3s, Cilium, ingress, observability, Velero'],
    ['Derive the SSE-C key in bash', 'three Secrets, then the location, read-only'],
    ['Wait for Velero to list the backups', 'up to 10 minutes'],
    ['Pick the newest complete Cluster backup', 'or the one named on the command line'],
    ['Velero restores it', 'SSE-C objects and Kopia volumes; up to 4 hours'],
    ['Swap in the console’s database', 'then the location becomes read-write'],
    ['Kwerft and Handoff', 'sign in with the accounts from the backup'],
  ];
  steps.forEach(([h, l], i) => {
    const y = top + i * (sh + sg);
    g.add(`<rect x="${sx}" y="${y}" width="${sw}" height="${sh}" rx="8" fill="${i === 6 ? TINT[C.s3] : C.surface}" stroke="${i === 6 ? C.s3 : C.axis}" stroke-width="1.5"/>`);
    g.add(`<circle cx="${sx + 20}" cy="${y + sh / 2}" r="11" fill="${C.band}" stroke="${C.axis}"/>`);
    g.text(sx + 20, y + sh / 2 + 4, String(i + 1), { size: 11.5, weight: 700, color: C.ink, anchor: 'middle' });
    g.text(sx + 42, y + 19, h, { size: 13, weight: 700, color: C.ink });
    g.text(sx + 42, y + 36, l, { size: 12 });
    if (i < steps.length - 1) g.add(`<line x1="${sx + 20}" y1="${y + sh / 2 + 11}" x2="${sx + 20}" y2="${y + sh + sg + sh / 2 - 11}" stroke="${C.axis}"/>`);
  });
  g.arrow(242, 130, sx - 4, 130);
  // Wrong key branch from step 3
  const y3 = top + 2 * (sh + sg) + sh / 2;
  g.add(`<path d="M${sx},${y3} L${sx - 20},${y3} L${sx - 20},${y3 + 150} L${242},${y3 + 150}" fill="none" stroke="${C.s2}" stroke-width="1.5" stroke-dasharray="5 4" marker-end="url(#x)"/>`);
  g.add(`<text transform="translate(${sx - 26},${y3 + 75}) rotate(-90)" font-size="11.5" font-weight="600" text-anchor="middle" fill="${C.s2}">wrong key</text>`);
  g.card(22, y3 + 116, 220, 92, { head: 'Exit code 60', lines: ['Velero lists no backups it', 'cannot read; the message', 'names the key file as a cause'], color: C.s2 });
  g.source('The e2e run checks it: same owner password, the marker file back, the secret’s SHA-256 unchanged.');
});
