import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Destination } from '../api/notifications';

// Fresh modules per test: the state lives in module scope.
let m: typeof import('./notifications.svelte');
let s: typeof import('./settings.svelte');
let events: typeof import('../api/events');

beforeEach(async () => {
  vi.resetModules();
  m = await import('./notifications.svelte');
  s = await import('./settings.svelte');
  events = await import('../api/events');
});
afterEach(() => vi.unstubAllGlobals());

/** Answers each "METHOD /path" with its [status, body]. */
function serve(routes: Record<string, [number, unknown?]>) {
  const f = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[`${init.method} ${url}`] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', f);
  return f;
}

const dest = (id: number, name: string): Destination => ({ id, name, kind: 'slack', url: 'https://hooks.slack.com/…bX9z', address_deleted: false, created_at: 1791276732, deliveries: [] });

it('fetches nothing in a build without the notifications module', async () => {
  const f = serve({});
  await m.load();
  events.dispatch('notifications.changed', {});
  expect(f).not.toHaveBeenCalled();
  expect(m.notifications.loaded).toBe(false);
});

it('loads the kinds and destinations, and refetches on notifications.changed', async () => {
  s.settings.value.features.notifications = true;
  serve({ 'GET /api/notifications': [200, { kinds: ['webhook', 'slack'], destinations: [dest(1, 'Team')] }] });
  await m.load();
  expect(m.notifications).toMatchObject({ kinds: ['webhook', 'slack'], loaded: true, error: '' });
  expect(m.notifications.destinations.map((d) => d.name)).toEqual(['Team']);

  serve({ 'GET /api/notifications': [200, { kinds: ['webhook', 'slack'], destinations: [dest(1, 'Team'), dest(2, 'Ops')] }] });
  events.dispatch('notifications.changed', {});
  await vi.waitFor(() => expect(m.notifications.destinations.map((d) => d.name)).toEqual(['Team', 'Ops']));
});

it('keeps a failed load apart from an empty list', async () => {
  s.settings.value.features.notifications = true;
  serve({ 'GET /api/notifications': [500, { error: { code: 'internal', message: 'Something went wrong.' } }] });
  await m.load();
  expect(m.notifications).toMatchObject({ loaded: false, error: 'Something went wrong.' });
});

it('adds a destination with its secret, and removes one', async () => {
  const f = serve({
    'POST /api/notifications/destinations': [201, { destination: dest(3, 'Hook'), secret: 'whsec_1' }],
    'DELETE /api/notifications/destinations/3': [204],
  });
  const created = await m.create({ name: 'Hook', kind: 'webhook', url: 'https://example.com/mailrules' });
  expect(created.secret).toBe('whsec_1');
  expect(JSON.parse(f.mock.calls[0][1].body as string)).toEqual({ name: 'Hook', kind: 'webhook', url: 'https://example.com/mailrules' });
  expect(m.notifications.destinations.map((d) => d.id)).toEqual([3]);

  await m.remove(3);
  expect(f.mock.calls[1][1].method).toBe('DELETE');
  expect(m.notifications.destinations).toEqual([]);
});
