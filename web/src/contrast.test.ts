import { expect, it } from 'vitest';

// Vitest empties .css imports, even ?raw, so the source is read from disk (Vitest runs from web/).
// The module name is a variable because the web tsconfig has no Node types.
const fsModule: string = 'node:fs';
const { readFileSync } = (await import(/* @vite-ignore */ fsModule)) as { readFileSync: (path: string, enc: 'utf8') => string };
const css = readFileSync('src/app.css', 'utf8');

// The light half of each light-dark() colour token in app.css.
const light = (token: string): string => {
  const m = css.match(new RegExp(`--color-${token}:\\s*light-dark\\((#[0-9a-f]{6})`, 'i'));
  if (!m) throw new Error(`no light value for --color-${token}`);
  return m[1];
};

const luminance = (hex: string) => {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  });
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
};

const ratio = (a: string, b: string) => {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x);
  return (hi + 0.05) / (lo + 0.05);
};

// WCAG AA: 3:1 for borders and status marks that carry meaning, 4.5:1 for small text.
it.each([
  ['line-input', 'surface', 3, 'input borders'],
  ['warn-strong', 'surface', 3, 'the Connecting status dot'],
  ['idle', 'surface', 3, 'the idle dot and the Left in Inbox bar'],
  ['muted', 'selected', 4.5, 'muted text on the selected nav item, e.g. its count'],
  ['muted', 'surface', 4.5, 'muted text on the page'],
])('light %s on %s reaches %d:1 (%s)', (fg, bg, min) => {
  expect(ratio(light(fg), light(bg))).toBeGreaterThanOrEqual(min);
});
