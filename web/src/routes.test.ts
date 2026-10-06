import { expect, it } from 'vitest';
import { routes, screens } from './routes';

it('gives every nav item a route and hides flagged-off screens', () => {
  expect(screens.length).toBeGreaterThan(0);
  for (const s of screens) expect(routes[s.path]).toBeDefined();
  expect(new Set(screens.map((s) => s.path)).size).toBe(screens.length);
  expect(screens.some((s) => s.path === '/plan')).toBe(false);
  expect(routes['*']).toBeDefined();
});
