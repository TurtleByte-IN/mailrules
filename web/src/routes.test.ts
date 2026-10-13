import { router } from 'svelte-spa-router';
import { afterEach, expect, it, vi } from 'vitest';
import { settings } from './lib/state/settings.svelte';
import { followPathRoute, hashAddress, routes, screensFor } from './routes';

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

// The app's own links are hash routes; the daemon serves the app at any path, so a screen's
// address without the hash (typed, bookmarked, reloaded) is moved into it.
it.each([
  ['/rules', '', '', '/#/rules'],
  ['/rules/', '', '', '/#/rules'],
  ['/activity', '?outcome=moved', '', '/#/activity?outcome=moved'],
  ['/accounts', '?mailbox_error=workspace_only', '', '/#/accounts?mailbox_error=workspace_only'],
  ['/', '', '#/review', undefined], // the summary email's links
  ['/', '', '#/activity?id=7', undefined],
  ['/', '?signin_error=denied', '', undefined],
  ['/', '', '', undefined],
  ['/rules', '', '#/review', undefined], // a hash route wins, as before
  ['/nowhere', '', '', undefined],
  ['/*', '', '', undefined],
])('moves %s%s%s to %s', (pathname, search, hash, to) => {
  expect(hashAddress({ pathname, search, hash })).toBe(to);
});

afterEach(() => history.replaceState(null, '', '/'));

it.each([
  ['a path', '/rules?keep=1', '/#/rules?keep=1', '/rules', 'keep=1'],
  ['a hash', '/#/review', '/#/review', '/review', ''],
])('opens the screen at %s address', async (_form, at, url, screen, querystring) => {
  history.replaceState(null, '', at);
  window.dispatchEvent(new Event('hashchange')); // the router reads the address the page opened at
  followPathRoute();
  expect(location.pathname + location.search + location.hash).toBe(url);
  await vi.waitFor(() => expect([router.location, router.querystring]).toEqual([screen, querystring]));
});
