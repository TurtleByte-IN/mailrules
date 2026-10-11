import { expect, it } from 'vitest';
import { settings } from './lib/state/settings.svelte';
import { routes, screensFor } from './routes';

it('gives every nav item a route and hides flagged-off screens', () => {
  const off = settings.value.features; // the placeholder until the daemon answers: every flag off
  const screens = screensFor(off);
  expect(screens.length).toBeGreaterThan(0);
  for (const s of screens) expect(routes[s.path]).toBeDefined();
  expect(new Set(screens.map((s) => s.path)).size).toBe(screens.length);
  expect(screens.some((s) => s.path === '/plan')).toBe(false);
  expect(screensFor({ ...off, billing: true }).some((s) => s.path === '/plan')).toBe(true);
  expect(routes['*']).toBeDefined();
});
