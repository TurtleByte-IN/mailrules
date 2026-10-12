// Fails when the production build's gzipped JS or CSS grows past its budget.
// Run after `vite build`: node scripts/bundle-budget.mjs
// Budgets are ~15% above the size measured on 2026-10-12 (JS 108.5 kB, CSS 8.1 kB gzip).
// Raise them on purpose, in the same change that justifies the growth.
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { gzipSync } from 'node:zlib';

const budget = { js: 125_000, css: 9_500, total: 134_000 }; // gzip bytes
const dir = new URL('../dist/assets/', import.meta.url).pathname;

const size = { js: 0, css: 0 };
for (const f of readdirSync(dir)) {
  const ext = f.split('.').pop();
  if (ext in size) size[ext] += gzipSync(readFileSync(join(dir, f)), { level: 9 }).length;
}
size.total = size.js + size.css;

let over = false;
for (const k of ['js', 'css', 'total']) {
  const bad = size[k] > budget[k];
  over ||= bad;
  console.log(`${bad ? 'OVER' : 'ok  '} ${k.padEnd(5)} ${(size[k] / 1000).toFixed(1)} kB gzip (budget ${(budget[k] / 1000).toFixed(1)} kB)`);
}
if (over) {
  console.error('bundle-budget: the web bundle is over budget; shrink it or raise the budget in web/scripts/bundle-budget.mjs with a reason');
  process.exit(1);
}
