import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Batch, CleanupCheck, CleanupCheckRow } from '../lib/api/cleanup';
import { dispatch } from '../lib/api/events';
import { accounts } from '../lib/state/accounts.svelte';
import { cleanup } from '../lib/state/cleanup.svelte';
import { toast } from '../lib/state/toast.svelte';
import Cleanup from './Cleanup.svelte';

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
let fetchMock: ReturnType<typeof vi.fn>;

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
  skipped: 0,
  ...over,
});

const checkRow = (over: Partial<CleanupCheckRow> = {}): CleanupCheckRow => ({
  index: 0,
  from: 'news@substack.com',
  subject: 'This week in Go',
  received_at: 1790000000,
  folder: 'INBOX',
  uid: 100,
  uidvalidity: 1,
  stage: 'condition',
  rule_id: 5,
  rule_name: 'Newsletters',
  actions: [{ type: 'move', folder: 'Newsletters' }],
  confidence: 1,
  selectable: true,
  review: false,
  reason: '',
  ...over,
});

const check = (over: Partial<CleanupCheck> = {}): CleanupCheck => ({
  id: 'chk1',
  account_id: 7,
  folder: 'INBOX',
  since: SINCE,
  limit: null,
  status: 'ready',
  done: 412,
  total: 412,
  model_calls: 80,
  tokens: 1200,
  cost_usd: 0.01,
  error: '',
  rows: [checkRow()],
  ...over,
});

const PAST = 'GET /api/batches?kind=cleanup';
const CHECK = 'GET /api/cleanup/check?account_id=7';
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
    scope: { accountId: '7', folder: 'INBOX', range: '90' },
    folders: [],
    check: null,
    excluded: new Set(),
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
    [CHECK]: [200, { check: null }],
    'POST /api/cleanup/check': [202, { check: check({ status: 'running', done: 0, total: 412, model_calls: 0, cost_usd: 0, rows: [] }) }],
    'DELETE /api/cleanup/check?account_id=7': [204],
    'POST /api/cleanup/run': [202, { batch: batch({ status: 'running', done: 0 }) }],
  };
  fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const runBody = () => {
  const call = fetchMock.mock.calls.find((c) => c[0] === '/api/cleanup/run');
  return call ? JSON.parse((call[1] as RequestInit).body as string) : null;
};

it('a failed load of past runs shows an alert, and Retry fetches again', async () => {
  routes[PAST] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Cleanup);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  routes[PAST] = page([]);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('No cleanup runs yet.')).toBeTruthy();
});

it('restores a ready check on mount: rows show, selectable rows are ticked, Needs-review rows are disabled with their reason', async () => {
  routes[CHECK] = [
    200,
    {
      check: check({
        rows: [
          checkRow({ index: 0, from: 'news@substack.com', subject: 'This week in Go', rule_name: 'Newsletters', confidence: 1 }),
          checkRow({ index: 1, from: 'ping@acme.io', subject: 'Nudge', rule_name: 'Cold sales', actions: [{ type: 'archive' }], confidence: 0.82, stage: 'decider', rule_id: null }),
          checkRow({ index: 2, from: 'hr@firm.com', subject: 'Offer', selectable: false, review: true, stage: 'decider', actions: [], rule_name: 'Offers', rule_id: 9, confidence: 0.5, reason: 'Waiting in Needs review' }),
          checkRow({ index: 3, from: 'pal@home.org', subject: 'Lunch?', selectable: false, stage: 'none', actions: [], rule_name: '', rule_id: null, confidence: 0, reason: 'No rule matched' }),
        ],
      }),
    },
  ];
  render(Cleanup);

  const first = await screen.findByRole('checkbox', { name: 'Sort news@substack.com · This week in Go' });
  expect((first as HTMLInputElement).checked).toBe(true);
  expect(screen.getByText('Move to Newsletters')).toBeTruthy();
  expect(screen.getByText('Archive')).toBeTruthy();
  expect(screen.getByText('100%')).toBeTruthy();
  expect(screen.getByText('82%')).toBeTruthy();

  const review = screen.getByRole('checkbox', { name: 'Sort hr@firm.com · Offer' }) as HTMLInputElement;
  expect(review.disabled).toBe(true);
  expect(screen.getByText('Waiting in Needs review')).toBeTruthy();

  expect(screen.getByText('2 selected of 4')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Sort 2 selected' })).toBeTruthy();

  // A Needs-review row is named so in the Action cell and shows how sure the model was; a left-alone row shows no confidence at all.
  const cells = (name: string) => within(screen.getByRole('checkbox', { name }).closest('tr') as HTMLElement).getAllByRole('cell');
  const offer = cells('Sort hr@firm.com · Offer');
  expect(offer[4].textContent).toBe('Needs review');
  expect(offer[5].textContent).toBe('50%');
  const lunch = cells('Sort pal@home.org · Lunch?');
  expect(lunch[4].textContent).toBe('—');
  expect(lunch[5].textContent).toBe('');

  // Returning to the page shows the same restored rows.
  render(Cleanup);
  expect((await screen.findAllByRole('checkbox', { name: 'Sort news@substack.com · This week in Go' })).length).toBeGreaterThan(0);
});

it('select all and select none change the count across every page', async () => {
  const rows = Array.from({ length: 60 }, (_, i) => checkRow({ index: i, from: `a${i}@x.com`, subject: 'Subject ' + i, uid: 100 + i }));
  routes[CHECK] = [200, { check: check({ rows }) }];
  render(Cleanup);

  expect(await screen.findByText('60 selected of 60')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Select none' }));
  expect(screen.getByText('0 selected of 60')).toBeTruthy();
  // Page two: the count is still the whole list, not the visible page.
  await fireEvent.click(screen.getByRole('button', { name: 'Next' }));
  expect(screen.getByText('0 selected of 60')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Select all' }));
  expect(screen.getByText('60 selected of 60')).toBeTruthy();
});

it('the rule filter narrows the visible rows without changing the selection count', async () => {
  routes[CHECK] = [
    200,
    {
      check: check({
        rows: [
          checkRow({ index: 0, from: 'news@substack.com', subject: 'Weekly Go', rule_name: 'Newsletters' }),
          checkRow({ index: 1, from: 'billing@shop.com', subject: 'Your receipt', rule_name: 'Receipts', uid: 101 }),
        ],
      }),
    },
  ];
  render(Cleanup);

  expect(await screen.findByText('Weekly Go')).toBeTruthy();
  expect(screen.getByText('Your receipt')).toBeTruthy();

  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: 'Receipts' } });
  expect(screen.queryByText('Weekly Go')).toBeNull();
  expect(screen.getByText('Your receipt')).toBeTruthy();
  // Filtering the view does not change what Sort would act on.
  expect(screen.getByText('2 selected of 2')).toBeTruthy();
});

it('pages the rows in fifties', async () => {
  const rows = Array.from({ length: 60 }, (_, i) => checkRow({ index: i, from: `a${i}@x.com`, subject: 'Subject ' + i, uid: 100 + i }));
  routes[CHECK] = [200, { check: check({ rows }) }];
  render(Cleanup);

  expect(await screen.findByText('Subject 0')).toBeTruthy();
  expect(screen.queryByText('Subject 55')).toBeNull();
  expect(screen.getByText('Page 1 of 2')).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Next' }));
  expect(screen.getByText('Subject 55')).toBeTruthy();
  expect(screen.queryByText('Subject 0')).toBeNull();
});

it('Sort reflects the ticked count and sends the unticked selectable indices as exclude', async () => {
  const rows = [
    checkRow({ index: 0, from: 'a@x.com', subject: 'One', uid: 100 }),
    checkRow({ index: 1, from: 'b@x.com', subject: 'Two', uid: 101 }),
    checkRow({ index: 2, from: 'c@x.com', subject: 'Three', uid: 102 }),
  ];
  routes[CHECK] = [200, { check: check({ rows }) }];
  render(Cleanup);

  expect(await screen.findByRole('button', { name: 'Sort 3 selected' })).toBeTruthy();
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort b@x.com · Two' }));
  expect(screen.getByRole('button', { name: 'Sort 2 selected' })).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Sort 2 selected' }));
  await vi.waitFor(() => expect(runBody()).not.toBeNull());
  expect(runBody()).toEqual({ account_id: 7, check_id: 'chk1', exclude: [1] });
});

it('a stale check shows a notice and refuses Sort', async () => {
  routes[CHECK] = [200, { check: check({ status: 'stale' }) }];
  render(Cleanup);

  expect(await screen.findByText('These results are out of date: the rules changed since this check. Check again before sorting.')).toBeTruthy();
  const sortBtn = screen.getByRole('button', { name: /^Sort / }) as HTMLButtonElement;
  expect(sortBtn.disabled).toBe(true);
});

it('resumes a running check: progress follows a check.progress event, then its rows load when ready', async () => {
  routes[CHECK] = [200, { check: check({ status: 'running', done: 0, model_calls: 0, cost_usd: 0, rows: [] }) }];
  render(Cleanup);

  expect(await screen.findByText('0 of 412 checked · 0 model calls · $0.00')).toBeTruthy();

  dispatch('check.progress', check({ status: 'running', done: 50, model_calls: 10, cost_usd: 0.005, rows: [] }));
  expect(await screen.findByText('50 of 412 checked · 10 model calls · $0.0050')).toBeTruthy();

  // Ready arrives without rows; the screen GETs the check to load them.
  routes[CHECK] = [200, { check: check({ rows: [checkRow({ subject: 'Loaded row' })] }) }];
  dispatch('check.progress', check({ status: 'ready', rows: [] }));
  expect(await screen.findByText('Loaded row')).toBeTruthy();
});

it('discard throws the check away and clears the table', async () => {
  routes[CHECK] = [200, { check: check() }];
  render(Cleanup);

  expect(await screen.findByText('This week in Go')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Discard check' }));
  await vi.waitFor(() => expect(screen.queryByText('This week in Go')).toBeNull());
  expect(screen.getByRole('button', { name: 'Check what would move' })).toBeTruthy();
});

it('reports the skipped count in the toast after a sort finishes', async () => {
  routes[CHECK] = [200, { check: check({ rows: [checkRow({ index: 0 }), checkRow({ index: 1, uid: 101, subject: 'Two' })] }) }];
  routes['POST /api/cleanup/run'] = [202, { batch: batch({ id: 9, status: 'running', done: 0, total: 2, skipped: 0 }) }];
  render(Cleanup);

  await fireEvent.click(await screen.findByRole('button', { name: 'Sort 2 selected' }));
  await screen.findByRole('progressbar', { name: 'Cleanup progress' });

  dispatch('batch.progress', batch({ id: 9, status: 'done', done: 7, total: 7, skipped: 3, actions: { done: 7, dry_run: 0, failed: 0, undone: 0 } }));
  await vi.waitFor(() => expect(toast.text).toBe('Cleanup done: 4 emails sorted, 3 skipped (no longer in the folder). Undo it as one batch below.'));
});

it('past runs render with their scope, counts and Undo; Show more follows the cursor', async () => {
  const cut = batch({ id: 2, status: 'failed', done: 90, since: null, account_id: null, actions: { done: 5, dry_run: 90, failed: 0, undone: 0 } });
  const undone = batch({ id: 1, status: 'undone', actions: { done: 0, dry_run: 0, failed: 0, undone: 310 } });
  routes[PAST] = page([batch(), cut], '2');
  routes[PAST + '&cursor=2'] = page([undone]);
  render(Cleanup);

  const name = 'me@icloud.com · Inbox · since ' + new Date(SINCE * 1000).toLocaleDateString([], { day: 'numeric', month: 'short' });
  const first = (await screen.findByText(name)).closest('.border-t') as HTMLElement;
  expect(first.textContent).toContain('actions: 310 done · 2 failed');
  expect(within(first).getByText('Done')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Undo batch ' + name })).toBeTruthy();

  const second = screen.getByText('a removed mailbox · Inbox · all time').closest('.border-t') as HTMLElement;
  expect(within(second).getByText('Cut short')).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Show more' }));
  expect(await screen.findByText('Undone')).toBeTruthy();
  expect(screen.getAllByRole('button', { name: /^Undo batch/ })).toHaveLength(2);
});

it('says so in place of Undo when a batch is too old or only a dry run', async () => {
  routes[PAST] = page([
    batch({ id: 6, folder: 'Recent', created_at: NOW - 29 * DAY }),
    batch({ id: 5, folder: 'Old', created_at: NOW - 30 * DAY - 1 }),
    batch({ id: 4, folder: 'Dry', actions: { done: 0, dry_run: 412, failed: 0, undone: 0 } }),
  ]);
  render(Cleanup);
  await screen.findByText(/ · Recent · /);
  const rowOf = (folder: string) => screen.getByText(new RegExp(' · ' + folder + ' · ')).closest('.border-t') as HTMLElement;

  expect(within(rowOf('Recent')).getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
  expect(within(rowOf('Old')).getByText('Too old to undo')).toBeTruthy();
  expect(within(rowOf('Dry')).getByText('Dry run: nothing to undo')).toBeTruthy();
});

it('a batch the daemon finds too old says so in a toast and keeps its row', async () => {
  const message = 'This was done more than 30 days ago, so it can no longer be undone.';
  routes[PAST] = page([batch()]);
  routes['POST /api/batches/3/undo'] = [409, { error: { code: 'too_old', message, path: '' } }];
  render(Cleanup);
  await fireEvent.click(await screen.findByRole('button', { name: /^Undo batch/ }));

  await vi.waitFor(() => expect(toast.text).toBe(message));
  expect(screen.getByText('Done')).toBeTruthy();
  expect(screen.getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
});
