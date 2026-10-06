import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Sender } from '../lib/api/senders';
import { rules } from '../lib/state/rules.svelte';
import { senders } from '../lib/state/senders.svelte';
import Senders from './Senders.svelte';

type Reply = [status: number, body: unknown];
let routes: Record<string, Reply>;

const sender = (value: string, over: Partial<Sender> = {}): Sender => ({
  type: 'address',
  value,
  name: '',
  messages: 0,
  last_seen_at: null,
  has_list_unsubscribe: false,
  verdict: null,
  rule_id: null,
  source: null,
  hits: 0,
  ...over,
});
const swiggy = sender('no-reply@swiggy.in', { name: 'Swiggy', messages: 64, last_seen_at: 1790000000 });
const quiet = sender('made.up@example.test', { verdict: 'keep', source: 'user' });
const jobs = sender('jobalerts.in', { type: 'domain', messages: 12, verdict: 'route', rule_id: 5, source: 'learned', hits: 3 });

const LIST = 'GET /api/senders?sort=volume';
const LEARNED = 'GET /api/senders?source=learned&limit=100';
const page = (items: Sender[]): Reply => [200, { items, next_cursor: null }];

beforeEach(() => {
  // The state lives in module scope; each test starts it over.
  Object.assign(senders, { status: 'loading', error: '', list: [], next: null, learned: [], sort: 'volume', q: '' });
  Object.assign(rules, { list: [{ id: 5, name: 'Recruiters' }] });
  routes = { [LIST]: page([swiggy, jobs, quiet]), [LEARNED]: page([jobs]) };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
      return new Response(JSON.stringify(body), { status });
    }),
  );
});
afterEach(() => vi.unstubAllGlobals());

it('a failed load shows an alert, and Retry fetches again', async () => {
  routes[LIST] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Senders);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  routes[LIST] = page([swiggy]);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Swiggy')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('no senders and nothing learned say so', async () => {
  routes[LIST] = page([]);
  routes[LEARNED] = page([]);
  render(Senders);
  expect(await screen.findByText('No senders found.')).toBeTruthy();
  expect(screen.getByText('Nothing learned yet.')).toBeTruthy();
});

it('shows names, addresses, domain rows, routing and the learned rules', async () => {
  render(Senders);
  expect(await screen.findByText('Swiggy')).toBeTruthy();
  expect(screen.getByText('no-reply@swiggy.in')).toBeTruthy();
  expect(screen.getByText('64 emails')).toBeTruthy();

  // A domain has no display name: it goes by its value and says what it covers.
  expect(screen.getByText('Domain: all its addresses')).toBeTruthy();
  expect((screen.getByLabelText('What happens to mail from jobalerts.in') as HTMLSelectElement).value).toBe('5');
  expect((screen.getByLabelText('What happens to mail from made.up@example.test') as HTMLSelectElement).value).toBe('keep');
  expect((screen.getByLabelText('What happens to mail from Swiggy') as HTMLSelectElement).value).toBe('auto');

  const learned = screen.getByRole('region', { name: 'Learned sender rules' });
  expect(learned.textContent).toContain('jobalerts.in → Recruiters');
  expect(screen.getByRole('button', { name: 'Forget jobalerts.in' })).toBeTruthy();
});
