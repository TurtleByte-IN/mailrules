import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Batch, Preview } from '../lib/api/cleanup';
import { dispatch } from '../lib/api/events';
import { day } from '../lib/format';
import { accounts } from '../lib/state/accounts.svelte';
import { cleanup } from '../lib/state/cleanup.svelte';
import { toast } from '../lib/state/toast.svelte';
import Cleanup from './Cleanup.svelte';

type Reply = [status: number, body: unknown];
let routes: Record<string, Reply>;

const SINCE = 1788739200;
const batch = (over: Partial<Batch> = {}): Batch => ({
  id: 3,
  kind: 'cleanup',
  status: 'done',
  total: 412,
  done: 412,
  created_at: 1790000000,
  actions: { done: 310, dry_run: 0, failed: 2, undone: 0 },
  account_id: 7,
  folder: 'INBOX',
  since: SINCE,
  tokens: 0,
  cost_usd: 0,
  ...over,
});
const row = (from: string, subject: string): Preview['groups'][number]['samples'][number] => ({
  from,
  subject,
  received_at: 1790000000,
  stage: 'condition',
  rule_id: 5,
  rule_name: 'Newsletters',
  confidence: 1,
  reason: '',
  review: false,
  actions: [],
});
const previewed: Preview = {
  total: 412,
  groups: [
    { key: 'rule:5', outcome: 'rule', rule_id: 5, rule_name: 'Newsletters', count: 298, samples: [row('news@substack.com', 'This week <b>in</b> Go')] },
    // Two sender rules without a rule: same outcome, both with a null rule_id.
    { key: 'sender:keep', outcome: 'rule', rule_id: null, rule_name: 'Sender rule: keep', count: 20, samples: [] },
    { key: 'sender:trash', outcome: 'rule', rule_id: null, rule_name: 'Sender rule: trash', count: 4, samples: [] },
    { key: 'model', outcome: 'model', rule_id: null, rule_name: '', count: 80, samples: [] },
    { key: 'none', outcome: 'none', rule_id: null, rule_name: '', count: 10, samples: [] },
  ],
  estimated_model_calls: 80,
  estimated_cost_usd: 0.01,
};

const PAST = 'GET /api/batches?kind=cleanup';
// The day after the batches above were made.
const NOW = 1790086400;
const DAY = 86400;
const page = (items: Batch[], next: string | null = null): Reply => [200, { items, next_cursor: next }];

beforeEach(() => {
  // Only the clock is set, so a batch's age does not depend on the day the tests run.
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW * 1000);
  // The state lives in module scope; each test starts it over.
  Object.assign(cleanup, {
    phase: 'idle',
    scope: { accountId: '', folder: 'INBOX', range: '90' },
    folders: [],
    preview: null,
    request: null,
    batch: null,
    batches: [],
    next: null,
    status: 'loading',
    error: '',
  });
  Object.assign(accounts, { list: [{ id: 7, label: 'me@icloud.com' }], loaded: true });
  routes = {
    [PAST]: page([]),
    'GET /api/accounts/7/folders': [200, { items: [{ name: 'INBOX', delimiter: '/', special_use: '' }] }],
    'POST /api/cleanup/preview': [200, previewed],
  };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
      return new Response(JSON.stringify(body), { status });
    }),
  );
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

it('a failed load of past runs shows an alert, and Retry fetches again', async () => {
  routes[PAST] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Cleanup);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  routes[PAST] = page([]);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('No cleanup runs yet.')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('there is no Run before a preview; the preview shows groups, samples, the estimate and the dry-run note', async () => {
  render(Cleanup);
  expect(await screen.findByText('No cleanup runs yet.')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Sort / })).toBeNull();

  await fireEvent.click(screen.getByRole('button', { name: 'Preview what would move' }));
  const run = (await screen.findByRole('button', { name: 'Sort 412 emails' })) as HTMLButtonElement;
  expect(run.disabled).toBe(false);

  expect(screen.getByText('Sender rule: keep')).toBeTruthy();
  expect(screen.getByText('Sender rule: trash')).toBeTruthy();
  expect(screen.getByText('For the model to decide')).toBeTruthy();
  expect(screen.getByText('Left where it is')).toBeTruthy();
  // A subject is text, never markup.
  expect(screen.getByText('news@substack.com').parentElement!.textContent).toBe('news@substack.com · This week <b>in</b> Go');
  expect(screen.getByText('About 80 model calls, around $0.01')).toBeTruthy();
  expect(screen.getByText('Dry-run is on: this run records what it would do and moves nothing.')).toBeTruthy();
});

it('past runs render with their scope, counts and Undo; Show more follows the cursor', async () => {
  const cut = batch({ id: 2, status: 'failed', done: 90, since: null, account_id: null, actions: { done: 5, dry_run: 90, failed: 0, undone: 0 } });
  const undone = batch({ id: 1, status: 'undone', actions: { done: 0, dry_run: 0, failed: 0, undone: 310 } });
  routes[PAST] = page([batch(), cut], '2');
  routes[PAST + '&cursor=2'] = page([undone]);
  render(Cleanup);

  const name = 'me@icloud.com · Inbox · since ' + day(SINCE);
  const first = (await screen.findByText(name)).closest('.border-t') as HTMLElement;
  expect(first.textContent).toContain('actions: 310 done · 2 failed');
  expect(within(first).getByText('Done')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Undo batch ' + name })).toBeTruthy();

  const second = screen.getByText('a removed mailbox · Inbox · all time').closest('.border-t') as HTMLElement;
  expect(second.textContent).toContain('90 dry-run');
  expect(within(second).getByText('Cut short')).toBeTruthy();
  expect(within(second).getByRole('button', { name: /^Undo batch/ })).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Show more' }));
  expect(await screen.findByText('Undone')).toBeTruthy();
  expect(screen.getAllByRole('button', { name: /^Undo batch/ })).toHaveLength(2);
  expect(screen.queryByRole('button', { name: 'Show more' })).toBeNull();
});

it('a run still going at load shows its progress with tokens and cost, and offers no Undo', async () => {
  routes[PAST] = page([batch({ status: 'running', done: 103, tokens: 1200, cost_usd: 0.02 })]);
  render(Cleanup);
  const bar = await screen.findByRole('progressbar', { name: 'Cleanup progress' });
  expect(bar.getAttribute('aria-valuenow')).toBe('25');
  expect(screen.getByText('103 of 412 sorted · 25% · 1,200 tokens · $0.02')).toBeTruthy();
  expect(screen.getByText('Running')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Undo batch/ })).toBeNull();

  // The event that ends the run also stops the poll the pickup started.
  dispatch('batch.progress', batch());
  expect(await screen.findByRole('status')).toBeTruthy();
  expect(screen.getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
});

it('says so in place of Undo when a batch is too old or only a dry run', async () => {
  routes[PAST] = page([
    batch({ id: 6, folder: 'Recent', created_at: NOW - 29 * DAY }),
    batch({ id: 5, folder: 'Old', created_at: NOW - 30 * DAY - 1 }),
    batch({ id: 4, folder: 'Dry', actions: { done: 0, dry_run: 412, failed: 0, undone: 0 } }),
  ]);
  render(Cleanup);
  await screen.findByText(/ · Recent · /);
  expect(screen.getByText('Kept for 30 days. Undo puts every email back where it was.')).toBeTruthy();
  const rowOf = (folder: string) => screen.getByText(new RegExp(' · ' + folder + ' · ')).closest('.border-t') as HTMLElement;

  expect(within(rowOf('Recent')).getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
  expect(within(rowOf('Old')).getByText('Too old to undo')).toBeTruthy();
  expect(within(rowOf('Dry')).getByText('Dry run: nothing to undo')).toBeTruthy();
  expect(screen.getAllByRole('button', { name: /^Undo batch/ })).toHaveLength(1);
});

it('a batch the daemon finds too old says so in a toast and keeps its row', async () => {
  // internal/api/server.go: what every undo route answers past store.UndoDays.
  const message = 'This was done more than 30 days ago, so it can no longer be undone.';
  routes[PAST] = page([batch()]);
  routes['POST /api/batches/3/undo'] = [409, { error: { code: 'too_old', message, path: '' } }];
  render(Cleanup);
  await fireEvent.click(await screen.findByRole('button', { name: /^Undo batch/ }));

  await vi.waitFor(() => expect(toast.text).toBe(message));
  expect(screen.getByText('Done')).toBeTruthy();
  expect(screen.getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
});
