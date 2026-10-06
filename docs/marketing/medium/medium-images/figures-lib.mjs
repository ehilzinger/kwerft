// Shared drawing helpers for the Kwerft Medium posts, copied from the
// hatchure-ios posts' figures so every diagram has the same style.
// Each post has its own <slug>-figures.mjs that imports this:
//
//   node <slug>-figures.mjs          # writes <name>.svg and <name>.html here
//   node render.mjs [prefix]         # headless Chrome at 2x → <name>.png
//
// Every label comes from the post it illustrates; measured numbers are only
// the ones the post quotes.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const OUT = process.env.FIG_OUT || path.dirname(fileURLToPath(import.meta.url));

export const C = { ink: '#1b2125', ink2: '#4a5358', muted: '#6b757b', grid: '#e6e8e4', axis: '#b9bfba',
  s1: '#2a78d6', s2: '#eb6834', s3: '#1baf7a', s4: '#eda100', band: '#f3f1ea', surface: '#ffffff' };
export const TINT = { [C.s1]: '#eaf2fc', [C.s2]: '#fdeee7', [C.s3]: '#e6f6ef', [C.s4]: '#fdf4de', [C.axis]: '#f3f1ea' };
export const FONT = `-apple-system, "Helvetica Neue", Helvetica, Arial, sans-serif`;
export const MONO = `ui-monospace, Menlo, monospace`;
export const W = 720;
export const esc = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;');

export function figure(name, h, draw) {
  let o = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${h}" viewBox="0 0 ${W} ${h}" font-family='${FONT}'>`;
  o += `<defs><marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0L10,5L0,10z" fill="${C.ink2}"/></marker>`;
  o += `<marker id="x" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse"><path d="M0,0L10,5L0,10z" fill="${C.s2}"/></marker></defs>`;
  o += `<rect width="${W}" height="${h}" fill="${C.surface}"/>`;
  const g = {
    add: s => { o += s; },
    title(t, sub) {
      o += `<text x="22" y="34" font-size="19" font-weight="700" fill="${C.ink}">${esc(t)}</text>`;
      if (sub) o += `<text x="22" y="58" font-size="13.5" fill="${C.ink2}">${esc(sub)}</text>`;
    },
    source(t) { o += `<text x="22" y="${h - 14}" font-size="11" fill="${C.muted}">${esc(t)}</text>`; },
    text(x, y, t, { size = 12.5, weight = 400, color = C.ink2, anchor = 'start', mono = false } = {}) {
      [].concat(t).forEach((line, i) => {
        o += `<text x="${x}" y="${y + i * (size + 5)}" font-size="${size}" font-weight="${weight}" text-anchor="${anchor}" fill="${color}"${mono ? ` font-family='${MONO}'` : ''}>${esc(line)}</text>`;
      });
    },
    // A card with an optional coloured stripe on the left.
    card(x, y, w, hh, { head, lines = [], color, fill, mono = false, headSize = 14 } = {}) {
      o += `<rect x="${x}" y="${y}" width="${w}" height="${hh}" rx="9" fill="${fill || (color ? TINT[color] : C.surface)}" stroke="${color || C.axis}" stroke-width="1.5"/>`;
      if (color) o += `<rect x="${x}" y="${y}" width="6" height="${hh}" rx="3" fill="${color}"/>`;
      const tx = x + (color ? 18 : 14);
      let ty = y + 24;
      if (head) { o += `<text x="${tx}" y="${ty}" font-size="${headSize}" font-weight="700" fill="${C.ink}"${mono ? ` font-family='${MONO}'` : ''}>${esc(head)}</text>`; ty += 21; }
      lines.forEach((l, i) => o += `<text x="${tx}" y="${ty + i * 17}" font-size="12.5" fill="${C.ink2}">${esc(l)}</text>`);
    },
    arrow(x1, y1, x2, y2, { color = C.ink2, dash = false, label, lx, ly, anchor = 'middle', marker = 'a', width = 1.5 } = {}) {
      o += `<line x1="${x1}" y1="${y1}" x2="${x2}" y2="${y2}" stroke="${color}" stroke-width="${width}"${dash ? ' stroke-dasharray="5 4"' : ''} marker-end="url(#${marker})"/>`;
      if (label) o += `<text x="${lx ?? (x1 + x2) / 2}" y="${ly ?? (y1 + y2) / 2 - 6}" font-size="11.5" text-anchor="${anchor}" fill="${C.ink2}">${esc(label)}</text>`;
    },
    chip(x, y, t, color, { w } = {}) {
      const width = w || t.length * 7.4 + 24;
      o += `<rect x="${x}" y="${y}" width="${width}" height="24" rx="12" fill="${TINT[color]}" stroke="${color}" stroke-width="1.25"/>`;
      o += `<text x="${x + width / 2}" y="${y + 16.5}" font-size="12" font-weight="600" text-anchor="middle" fill="${C.ink}">${esc(t)}</text>`;
      return width;
    },
    cross(x, y, color = C.s2) {
      o += `<path d="M${x - 6},${y - 6}L${x + 6},${y + 6}M${x + 6},${y - 6}L${x - 6},${y + 6}" stroke="${color}" stroke-width="2.5" stroke-linecap="round"/>`;
    },
  };
  draw(g);
  o += '</svg>';
  fs.writeFileSync(path.join(OUT, name + '.svg'), o);
  fs.writeFileSync(path.join(OUT, name + '.html'), `<html><body style="margin:0;background:#fff">${o}</body></html>`);
  console.log(name, W + 'x' + h);
}

