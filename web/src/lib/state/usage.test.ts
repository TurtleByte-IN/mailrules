import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { ModelUsage, StatsUsage } from '../api/usage';

// Fresh modules per test: the state lives in module scope.
let m: typeof import('./usage.svelte');
let events: typeof import('../api/events');

beforeEach(async () => {
  vi.resetModules();
  m = await import('./usage.svelte');
  events = await import('../api/events');
});
afterEach(() => vi.unstubAllGlobals());

function respond(status: number, body: unknown) {
  const f = vi.fn(async (_url: string, _init: RequestInit) => new Response(JSON.stringify(body), { status }));
  vi.stubGlobal('fetch', f);
  return f;
}

const model = (purpose: ModelUsage['purpose'], calls: number): ModelUsage => ({
  provider: 'p',
  model: 'm',
  purpose,
  calls,
  tokens_in: 0,
  tokens_out: 0,
  cost_usd: 0,
});
const stats = (over: Partial<StatsUsage> = {}): StatsUsage => ({
  range: 'month',
  since: 1790812800,
  emails: 0,
  calls: 0,
  cost_usd: 0,
  days: [],
  by_rule: [],
  by_model: [],
  without_model: 0,
  ...over,
});

it.each([
  ['nothing sorted yet', stats(), { emails: 0, freePct: 0, decide: 0, escalate: 0, calls: 0, costUsd: 0 }],
  [
    'calls and cost are the API totals, composer included',
    stats({ emails: 200, without_model: 150, calls: 53, cost_usd: 1.75, by_model: [model('decide', 40), model('escalate', 10), model('compose', 3)] }),
    { emails: 200, freePct: 75, decide: 40, escalate: 10, calls: 53, costUsd: 1.75 },
  ],
  [
    'one purpose spread over two models is summed',
    stats({ emails: 3, without_model: 1, calls: 3, by_model: [model('decide', 1), model('decide', 2)] }),
    { emails: 3, freePct: 33, decide: 3, escalate: 0, calls: 3, costUsd: 0 },
  ],
])('totals: %s', (_name, s, want) => expect(m.totals(s)).toEqual(want));

it('load asks GET /api/stats/usage and keeps the answer', async () => {
  const f = respond(200, stats({ emails: 9 }));
  await m.load();
  expect(f.mock.calls[0][0]).toBe('/api/stats/usage');
  expect(m.usage).toMatchObject({ status: 'ready', summary: { emails: 9 } });
});

it.each([
  [501, { error: { code: 'not_implemented', message: 'This part of MailRules is not built yet.' } }, 'not_built'],
  [500, { error: { code: 'internal', message: 'Something went wrong.' } }, 'error'],
])('a %i gives the %s state', async (status, body, want) => {
  respond(status, body);
  await m.load();
  expect(m.usage).toMatchObject({ status: want, error: body.error.message, summary: null });
});

it('usage.updated refetches once numbers are showing, and not before', async () => {
  const f = respond(200, stats({ calls: 1 }));
  events.dispatch('usage.updated', {});
  expect(f).not.toHaveBeenCalled();

  await m.load();
  respond(200, stats({ calls: 2 }));
  events.dispatch('usage.updated', {});
  await vi.waitFor(() => expect(m.usage.summary!.calls).toBe(2));
});
