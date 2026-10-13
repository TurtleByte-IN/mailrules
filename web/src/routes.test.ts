import { expect, it } from 'vitest';
import { settings } from './lib/state/settings.svelte';
import { routes, screensFor } from './routes';

const off = settings.value.features; // the placeholder until the daemon answers: every flag off
const paths = (...args: Parameters<typeof screensFor>) => screensFor(...args).map((s) => s.path);

it('gives every nav item a route and hides flagged-off screens', () => {
  const screens = screensFor(off, 'owner');
  expect(screens.length).toBeGreaterThan(0);
  for (const s of screens) expect(routes[s.path]).toBeDefined();
  expect(new Set(screens.map((s) => s.path)).size).toBe(screens.length);
  expect(paths(off, 'owner')).not.toContain('/plan');
  expect(paths({ ...off, billing: true }, 'owner')).toContain('/plan');
  expect(paths(off, 'owner')).not.toContain('/members');
  expect(paths({ ...off, team: true }, 'member')).toContain('/members');
  expect(routes['*']).toBeDefined();
});

it.each([
  ['owner', true, true],
  ['admin', false, true],
  ['member', false, false],
] as const)('offers an %s Plan and billing: %s, and Add rules: %s', (role, plan, compose) => {
  const shown = paths({ ...off, billing: true, team: true }, role);
  expect(shown.includes('/plan')).toBe(plan);
  expect(shown.includes('/compose')).toBe(compose);
  expect(shown).toContain('/members');
});
