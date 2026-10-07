// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Renders every <name>.html next to this file (or those starting with the
// given prefix) to <name>.png with headless Chrome at 2x, sized from the SVG.
import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const DIR = path.dirname(fileURLToPath(import.meta.url));
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const prefix = process.argv[2] || '';
for (const f of fs.readdirSync(DIR).filter(f => f.endsWith('.html') && f.startsWith(prefix))) {
  const html = fs.readFileSync(path.join(DIR, f), 'utf8');
  const [, w, h] = html.match(/<svg[^>]* width="(\d+)" height="(\d+)"/);
  const png = path.join(DIR, f.replace(/\.html$/, '.png'));
  execFileSync(CHROME, ['--headless', '--disable-gpu', '--hide-scrollbars', `--window-size=${w},${h}`,
    '--force-device-scale-factor=2', `--screenshot=${png}`, 'file://' + path.join(DIR, f)], { stdio: 'ignore' });
  console.log(path.basename(png), `${w * 2}x${h * 2}`);
}
