// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Diagrams for medium-impersonation-2026-10-06.md (Kubernetes RBAC as the
// authorization layer, write-only secrets). Every label comes from the post;
// nothing here is measured data.
//
//   node rbac-figures.mjs && node render.mjs rbac-
import { figure, C, TINT, W, esc } from './figures-lib.mjs';

// A chip with a fixed width, its text centred (the lib's chip, sized by us).
const chipW = (g, x, y, t, color, w) => g.chip(x, y, t, color, { w });

// 1. The request path: whose permissions each step uses.
figure('rbac-fig1-request-path', 700, g => {
  g.title('The console asks Kubernetes, as you',
    'A change from the browser to the cluster, and whose permissions each step uses');
  const x = 22, w = 430, top = 96, rh = 74, gap = 20, cx = 478, cw = 220;
  g.text(cx, top - 12, 'Whose permissions', { size: 12, weight: 600, color: C.muted });
  const rows = [
    { head: 'Browser', lines: ['A developer deploys an App in a project'],
      chip: ['the signed-in user', C.axis] },
    { head: 'Console API', lines: ['Checks the session and the role, to refuse', 'early with a clear message'],
      chip: ['console’s own checks', C.axis] },
    { head: 'Impersonated write', lines: ['The App is sent as user kwerft:<email>, groups', 'kwerft:role:developer, system:authenticated'],
      chip: ['as the user', C.s1], note: 'single-object reads too' },
    { head: 'API server: RBAC', lines: ['The final gate: the project’s RoleBindings', 'decide, or it answers 403'],
      chip: ['Kubernetes decides', C.s3] },
    { head: 'Reconciler', lines: ['Renders Deployment, Service, HTTPRoute and', 'CiliumNetworkPolicy with server-side apply'],
      chip: ['its own permissions', C.s4] },
    { head: 'Polled lists', lines: ['Read from the informer cache, filtered to', 'the user’s projects (scope.go)'],
      chip: ['the console, filtered', C.s2], note: 'a second path that must mirror RBAC' },
  ];
  rows.forEach((r, i) => {
    const y = top + i * (rh + gap);
    g.card(x, y, w, rh, { head: r.head, lines: r.lines, color: r.chip[1] === C.axis ? undefined : r.chip[1], headSize: 13.5 });
    const cy = y + rh / 2 - 12 - (r.note ? 8 : 0);
    chipW(g, cx, cy, r.chip[0], r.chip[1], cw);
    if (r.note) g.text(cx + cw / 2, cy + 42, r.note, { size: 12, anchor: 'middle' });
    g.arrow(x + w + 4, cy + 12, cx - 6, cy + 12, { color: C.axis });
    if (i < rows.length - 1) g.arrow(x + w / 2, y + rh, x + w / 2, y + rh + gap - 1);
  });
  g.source('As groups, the console’s service account may claim only the four role groups and system:authenticated.');
});

// 2. Role × resource reach matrix.
figure('rbac-fig2-reach-matrix', 472, g => {
  g.title('What each role reaches in a project',
    'A Team project, or a Members project listing the user with that role. Last column: not listed.');
  const lx = 22, c0 = 254, cw = 90, top = 92, rh = 42;
  const cols = ['Owner', 'Admin', 'Developer', 'Viewer', 'Not listed'];
  cols.forEach((t, i) => g.text(c0 + i * cw + (cw - 6) / 2, top, t, { size: 12, weight: 600, color: C.muted, anchor: 'middle' }));
  const WR = ['write', C.s3], RD = ['read', C.s1], NO = ['no', C.axis], SET = ['set', C.s3], OPEN = ['yes', C.s3],
    REV = ['reveal *', C.s4], LIST = ['names', C.s1];
  const rows = [
    ['Apps, tasks, volumes, domains', [WR, WR, WR, RD, NO]],
    ['Pods and logs', [RD, RD, RD, RD, NO]],
    ['A shell in a pod', [OPEN, OPEN, OPEN, NO, NO]],
    ['Secret key names', [RD, RD, RD, RD, NO]],
    ['Set a secret value', [SET, SET, SET, NO, NO]],
    ['Read a secret value', [REV, REV, NO, NO, NO]],
    ['Project names', [LIST, LIST, LIST, LIST, LIST]],
  ];
  rows.forEach(([what, cells], i) => {
    const y = top + 14 + i * rh;
    g.add(`<rect x="${lx}" y="${y}" width="${W - 44}" height="${rh - 6}" rx="8" fill="${i % 2 ? C.surface : C.band}"/>`);
    g.text(lx + 14, y + 23, what, { size: 13, color: C.ink });
    cells.forEach(([t, color], j) => chipW(g, c0 + j * cw, y + (rh - 6) / 2 - 12, t, color, cw - 6));
  });
  const fy = top + 14 + rows.length * rh + 16;
  g.text(22, fy, ['* One value by name, after their password or an authenticator code, audited; never with an API token.',
    'No role reaches Secrets through its cluster-wide role. Project names stay listable for every role.'], { size: 12.5 });
  g.source('Kubernetes RBAC from the chart’s roles and the reconcilers’ RoleBindings, held by the isolation suite.');
});

// 3. The secret write path: who sees the value.
figure('rbac-fig3-secret-paths', 776, g => {
  g.title('Who sees a secret value',
    'Setting a key in a secret set, and the ways the value is read afterwards');
  const x = 22, w = 456, top = 96, rh = 74, gap = 12, cx = 494, cw = 204;
  g.text(cx, top - 12, 'Sees the value', { size: 12, weight: 600, color: C.muted });
  const rows = [
    { head: 'Developer and their browser', lines: ['Sends the value once; gets back the key’s', 'name, the time and who set it'], chip: ['never gets it back', C.s3] },
    { head: 'Console API', lines: ['Patches as the user, as PartialObjectMetadata;', 'the audit entry names the set and key'], chip: ['only in the request', C.s4] },
    { head: 'API server and etcd', lines: ['Stores it, encrypted at rest by k3s. RBAC:', 'patch on exactly the sets’ Secrets, no get'], chip: ['stores it', C.s1] },
    { head: 'SecretSet reconciler', lines: ['Own permissions: copies key names, times', 'and authors into the set’s status'], chip: ['reads, writes names', C.s4] },
    { head: 'App reconciler', lines: ['Own permissions: hashes referenced values into', 'kwerft.dev/secrets-hash; a change rolls the App'], chip: ['reads, writes a hash', C.s4] },
    { head: 'Owner or admin', lines: ['Reveal: password or authenticator code, get', 'by name (kwerft:secret-sets-read), audited'], chip: ['one value, on purpose', C.s2] },
    { head: 'Viewers, other projects', lines: ['Key names in their own projects only;', 'another project’s sets answer 403'], chip: ['never', C.s3] },
  ];
  rows.forEach((r, i) => {
    const y = top + i * (rh + gap);
    g.card(x, y, w, rh, { head: r.head, lines: r.lines, color: r.chip[1], headSize: 13.5 });
    chipW(g, cx, y + rh / 2 - 12, r.chip[0], r.chip[1], cw);
  });
  const fy = top + rows.length * (rh + gap) + 14;
  g.text(22, fy, ['Not covered: anyone with that Role and a raw kubeconfig could ask for the full patch answer (the', 'proxy refuses Secrets), and a shell in the pod reads the running process’s environment.'], { size: 12.5 });
  g.source('Secret sets are in the 0.6 release candidates. The older write-only settings still receive the value in the patch answer.');
});
