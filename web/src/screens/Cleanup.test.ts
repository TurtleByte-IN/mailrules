import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Batch, CleanupCheck, CleanupCheckRow } from '../lib/api/cleanup';
import { dispatch } from '../lib/api/events';
import { accounts } from '../lib/state/accounts.svelte';
import { cleanup } from '../lib/state/cleanup.svelte';
import { rules } from '../lib/state/rules.svelte';
import { settings } from '../lib/state/settings.svelte';
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
  limit: 2000,
  matched: 412,
  run_id: null,
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
  limit: 2000,
  status: 'ready',
  done: 412,
  total: 412,
  matched: 412,
  model_calls: 80,
  tokens: 1200,
  cost_usd: 0.01,
  error: '',
  rows: [checkRow()],
  exclude: [],
  rule_ids: null,
  ...over,
});

const PAST = 'GET /api/batches?kind=cleanup';
const CHECK = 'GET /api/cleanup/checks';
// The day after the batches above were made.
const NOW = 1790086400;
const DAY = 86400;
const page = (items: Batch[], next: string | null = null): Reply => [200, { items, next_cursor: next }];

beforeEach(() => {
  // The limits GET /api/settings reports.
  Object.assign(settings.value, { limits: { test_default: 200, test_max: 2000, check_max: 2000 } });
  // Only the clock is set, so a batch's age does not depend on the day the tests run.
  vi.useFakeTimers({ toFake: ['Date'] });
  vi.setSystemTime(NOW * 1000);
  // The state lives in module scope; each test starts it over.
  Object.assign(cleanup, {
    phase: 'idle',
    scope: { accountId: '7', folder: 'INBOX', mode: 'newest', newest: 200, days: 90 },
    folders: [],
    mailboxes: ['7'],
    ruleIds: null,
    lanes: [],
    batches: [],
    next: null,
    status: 'loading',
    error: '',
  });
  Object.assign(accounts, { list: [{ id: 7, label: 'me@icloud.com' }], loaded: true });
  routes = {
    [PAST]: page([]),
    'GET /api/accounts/7/folders': [200, { items: [{ name: 'INBOX', delimiter: '/', special_use: '' }] }],
    [CHECK]: [200, { checks: [] }],
    'POST /api/cleanup/check': [202, { checks: [check({ status: 'running', done: 0, total: 412, model_calls: 0, cost_usd: 0, rows: [] })] }],
    'DELETE /api/cleanup/check?account_id=7': [204],
    'PUT /api/cleanup/check/selection': [204],
    'POST /api/cleanup/run': [202, { batches: [batch({ status: 'running', done: 0 })] }],
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

// The emails open under the chart on demand: the toggle is the one button on the screen that says whether it is open.
const openList = async () => fireEvent.click(await screen.findByRole('button', { expanded: false }));
const sortButton = () => screen.getByRole<HTMLButtonElement>('button', { name: /^Sort \d/ });

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
      checks: [
        check({
          rows: [
            checkRow({ index: 0, from: 'news@substack.com', subject: 'This week in Go', rule_name: 'Newsletters', confidence: 1 }),
            checkRow({ index: 1, from: 'ping@acme.io', subject: 'Nudge', rule_name: 'Cold sales', actions: [{ type: 'archive' }], confidence: 0.82, stage: 'decider', rule_id: null }),
            checkRow({ index: 2, from: 'hr@firm.com', subject: 'Offer', selectable: false, review: true, stage: 'decider', actions: [], rule_name: 'Offers', rule_id: 9, confidence: 0.5, reason: 'Waiting in Needs review' }),
            checkRow({ index: 3, from: 'pal@home.org', subject: 'Lunch?', selectable: false, stage: 'none', actions: [], rule_name: '', rule_id: null, confidence: 0, reason: 'No rule matched' }),
          ],
        }),
      ],
    },
  ];
  render(Cleanup);

  await openList();
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
  expect(sortButton().textContent).toMatch(/^\s*Sort 2\b/);

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
  await openList();
  expect(screen.getAllByRole('checkbox', { name: 'Sort news@substack.com · This week in Go' }).length).toBe(2);
});

it.each([
  [true, 'Move to MailRules Trash'],
  [false, 'Move to Trash'],
])('with trash_to_folder %s, a row that trashes names where Sort will move it: %s', async (on, text) => {
  settings.value.trash_to_folder = on;
  try {
    routes[CHECK] = [200, { checks: [check({ rows: [checkRow({ subject: 'Spam', rule_name: 'Block', actions: [{ type: 'trash' }] })] })] }];
    render(Cleanup);
    await openList();
    const row = (await screen.findByRole('checkbox', { name: 'Sort news@substack.com · Spam' })).closest('tr') as HTMLElement;
    expect(within(row).getAllByRole('cell')[4].textContent).toBe(text);
  } finally {
    settings.value.trash_to_folder = true;
  }
});

const saves = () => fetchMock.mock.calls.filter((c) => c[0] === '/api/cleanup/check/selection').map((c) => JSON.parse((c[1] as RequestInit).body as string));
const three = () => [checkRow({ index: 0, from: 'a@x.io', subject: 'One' }), checkRow({ index: 1, from: 'b@x.io', subject: 'Two' }), checkRow({ index: 2, from: 'c@x.io', subject: 'Three' })];

it("restores the user's ticks from the daemon on mount, after a reload or a return to the page", async () => {
  routes[CHECK] = [200, { checks: [check({ rows: three(), exclude: [1] })] }];
  render(Cleanup);

  await openList();
  expect((await screen.findByRole<HTMLInputElement>('checkbox', { name: 'Sort a@x.io · One' })).checked).toBe(true);
  expect(screen.getByRole<HTMLInputElement>('checkbox', { name: 'Sort b@x.io · Two' }).checked).toBe(false);
  expect(screen.getByRole<HTMLInputElement>('checkbox', { name: 'Sort c@x.io · Three' }).checked).toBe(true);
  expect(screen.getByText('2 selected of 3')).toBeTruthy();
  expect(sortButton().textContent).toMatch(/^\s*Sort 2\b/);
  expect(saves()).toEqual([]); // restoring is not a change: nothing is sent back

  // Sort sends its own list, which is the same as the restored ticks.
  await fireEvent.click(sortButton());
  await vi.waitFor(() => expect(runBody()).toEqual({ runs: [{ account_id: 7, check_id: 'chk1', exclude: [1] }] }));
});

it('a burst of tick changes saves once, with the latest list, after a short wait', async () => {
  routes[CHECK] = [200, { checks: [check({ rows: three() })] }];
  render(Cleanup);
  await openList();
  await fireEvent.click(await screen.findByRole('checkbox', { name: 'Sort a@x.io · One' }));
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort b@x.io · Two' }));
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort a@x.io · One' })); // ticked again
  expect(saves()).toEqual([]); // still waiting for the burst to end

  await vi.waitFor(() => expect(saves()).toEqual([{ account_id: 7, check_id: 'chk1', exclude: [1] }]));
  await fireEvent.click(screen.getByRole('button', { name: 'Select none' }));
  await vi.waitFor(() => expect(saves().at(-1)).toEqual({ account_id: 7, check_id: 'chk1', exclude: [0, 1, 2] }));
  await fireEvent.click(screen.getByRole('button', { name: 'Select all' }));
  await vi.waitFor(() => expect(saves().at(-1)).toEqual({ account_id: 7, check_id: 'chk1', exclude: [] }));
  expect(saves()).toHaveLength(3);
});

it("a failed save says why, keeps the ticks as they are and is tried again on the next change", async () => {
  routes[CHECK] = [200, { checks: [check({ rows: three() })] }];
  routes['PUT /api/cleanup/check/selection'] = [409, { error: { code: 'preview_stale', message: 'This check is no longer current. Run a new check, then sort.' } }];
  render(Cleanup);
  await openList();
  await fireEvent.click(await screen.findByRole('checkbox', { name: 'Sort a@x.io · One' }));
  await vi.waitFor(() => expect(toast.text).toBe('This check is no longer current. Run a new check, then sort.'));
  expect(screen.getByRole<HTMLInputElement>('checkbox', { name: 'Sort a@x.io · One' }).checked).toBe(false);
  expect(screen.getByText('2 selected of 3')).toBeTruthy();

  routes['PUT /api/cleanup/check/selection'] = [204];
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort b@x.io · Two' }));
  await vi.waitFor(() => expect(saves()).toHaveLength(2));
  expect(saves()[1]).toEqual({ account_id: 7, check_id: 'chk1', exclude: [0, 1] }); // the whole current list, not only the change
});

it('select all and select none change the count across every page', async () => {
  const rows = Array.from({ length: 60 }, (_, i) => checkRow({ index: i, from: `a${i}@x.com`, subject: 'Subject ' + i, uid: 100 + i }));
  routes[CHECK] = [200, { checks: [check({ rows })] }];
  render(Cleanup);

  await openList();
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
      checks: [
        check({
          rows: [
            checkRow({ index: 0, from: 'news@substack.com', subject: 'Weekly Go', rule_name: 'Newsletters' }),
            checkRow({ index: 1, from: 'billing@shop.com', subject: 'Your receipt', rule_name: 'Receipts', uid: 101 }),
          ],
        }),
      ],
    },
  ];
  render(Cleanup);

  await openList();
  expect(await screen.findByText('Weekly Go')).toBeTruthy();
  expect(screen.getByText('Your receipt')).toBeTruthy();

  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: 'Receipts' } });
  expect(screen.queryByText('Weekly Go')).toBeNull();
  expect(screen.getByText('Your receipt')).toBeTruthy();
  // Filtering the view does not change what Sort would act on.
  expect(screen.getByText('2 selected of 2')).toBeTruthy();
});

it('Select none and Select all act on the rows the rule filter shows, and leave the others as they are', async () => {
  routes[CHECK] = [
    200,
    {
      checks: [
        check({
          rows: [
            checkRow({ index: 0, from: 'news@substack.com', subject: 'Weekly Go', rule_name: 'Newsletters' }),
            checkRow({ index: 1, from: 'billing@shop.com', subject: 'Your receipt', rule_name: 'Receipts', uid: 101 }),
            checkRow({ index: 2, from: 'news@go.dev', subject: 'Go news', rule_name: 'Newsletters', uid: 102 }),
          ],
        }),
      ],
    },
  ];
  render(Cleanup);

  await openList();
  expect(await screen.findByText('3 selected of 3')).toBeTruthy();
  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: 'Newsletters' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Select none' }));
  expect(screen.getByText('1 selected of 3')).toBeTruthy();

  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: '' } });
  expect(screen.getByRole<HTMLInputElement>('checkbox', { name: /Your receipt/ }).checked).toBe(true);
  expect(screen.getByRole<HTMLInputElement>('checkbox', { name: /Weekly Go/ }).checked).toBe(false);

  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: 'Receipts' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Select none' }));
  await fireEvent.change(screen.getByRole('combobox', { name: 'Rule' }), { target: { value: 'Newsletters' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Select all' }));
  expect(screen.getByText('2 selected of 3')).toBeTruthy();
});

it('pages the rows in fifties', async () => {
  const rows = Array.from({ length: 60 }, (_, i) => checkRow({ index: i, from: `a${i}@x.com`, subject: 'Subject ' + i, uid: 100 + i }));
  routes[CHECK] = [200, { checks: [check({ rows })] }];
  render(Cleanup);

  await openList();
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
  routes[CHECK] = [200, { checks: [check({ rows })] }];
  render(Cleanup);

  await openList();
  expect(sortButton().textContent).toMatch(/^\s*Sort 3\b/);
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort b@x.com · Two' }));
  expect(sortButton().textContent).toMatch(/^\s*Sort 2\b/);

  await fireEvent.click(sortButton());
  await vi.waitFor(() => expect(runBody()).not.toBeNull());
  expect(runBody()).toEqual({ runs: [{ account_id: 7, check_id: 'chk1', exclude: [1] }] });
});

it('a stale check shows a notice and refuses Sort', async () => {
  routes[CHECK] = [200, { checks: [check({ status: 'stale' })] }];
  render(Cleanup);

  expect(await screen.findByText('These results are out of date: the rules changed since this check. Check again before sorting.')).toBeTruthy();
  const sortBtn = screen.getByRole('button', { name: /^Sort / }) as HTMLButtonElement;
  expect(sortBtn.disabled).toBe(true);
});

it('resumes a running check: progress follows a check.progress event, then its rows load when ready', async () => {
  routes[CHECK] = [200, { checks: [check({ status: 'running', done: 0, model_calls: 0, cost_usd: 0, rows: [] })] }];
  render(Cleanup);

  expect(await screen.findByText('0 of 412 checked · 0 model calls · $0.00')).toBeTruthy();

  dispatch('check.progress', check({ status: 'running', done: 50, model_calls: 10, cost_usd: 0.005, rows: [] }));
  expect(await screen.findByText('50 of 412 checked · 10 model calls · $0.0050')).toBeTruthy();

  // Ready arrives without rows; the screen GETs the check to load them.
  routes[CHECK] = [200, { checks: [check({ rows: [checkRow({ subject: 'Loaded row' })] })] }];
  dispatch('check.progress', check({ status: 'ready', rows: [] }));
  await openList();
  expect(screen.getByText('Loaded row')).toBeTruthy();
});

it('discard throws the check away and clears the table', async () => {
  routes[CHECK] = [200, { checks: [check()] }];
  render(Cleanup);

  await openList();
  expect(await screen.findByText('This week in Go')).toBeTruthy();
  await fireEvent.click(screen.getByRole('button', { name: 'Discard check' }));
  await vi.waitFor(() => expect(screen.queryByText('This week in Go')).toBeNull());
  expect(screen.getByRole('button', { name: 'Check what would move' })).toBeTruthy();
});

it('reports the skipped count in the toast after a sort finishes', async () => {
  routes[CHECK] = [200, { checks: [check({ rows: [checkRow({ index: 0 }), checkRow({ index: 1, uid: 101, subject: 'Two' })] })] }];
  routes['POST /api/cleanup/run'] = [202, { batches: [batch({ id: 9, status: 'running', done: 0, total: 2, skipped: 0 })] }];
  render(Cleanup);

  await fireEvent.click(await screen.findByRole('button', { name: /^Sort 2\b/ }));
  await screen.findByRole('progressbar', { name: 'Cleanup progress' });

  dispatch('batch.progress', batch({ id: 9, status: 'done', done: 7, total: 7, skipped: 3, actions: { done: 7, dry_run: 0, failed: 0, undone: 0 } }));
  await vi.waitFor(() => expect(toast.text).toBe('Cleanup done: 4 emails sorted, 3 skipped (no longer in the folder). Undo it as one batch below.'));
});

it('past runs render with their scope, counts and Undo; Show more follows the cursor', async () => {
  const cut = batch({ id: 2, status: 'failed', done: 90, since: null, limit: null, matched: null, account_id: null, actions: { done: 5, dry_run: 90, failed: 0, undone: 0 } });
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
  await fireEvent.click(screen.getByRole('button', { name: 'Yes, undo this batch' }));

  await vi.waitFor(() => expect(toast.text).toBe(message));
  expect(screen.getByText('Done')).toBeTruthy();
  expect(screen.getByRole('button', { name: /^Undo batch/ })).toBeTruthy();
});

// Undo moves real mail even with dry-run on, so a batch asks first, with the count when it is known.
it.each([
  ['every action real and in effect: the count', { done: 14, skipped: 0, actions: { done: 18, dry_run: 0, failed: 0, undone: 0 } }, 'Move 14 emails back to where they were?'],
  ['some actions failed: no count', { actions: { done: 310, dry_run: 0, failed: 2, undone: 0 } }, 'Put every email this batch moved back where it was?'],
])('Undo batch asks first (%s); Cancel moves nothing and gives focus back', async (_name, over, question) => {
  routes[PAST] = page([batch(over)]);
  render(Cleanup);
  const open = await screen.findByRole('button', { name: /^Undo batch/ });
  open.focus();
  await fireEvent.click(open);

  const ask = screen.getByRole('alertdialog', { name: question });
  expect(ask.textContent).toContain('Dry-run does not stop an undo');
  expect(document.activeElement).toBe(within(ask).getByRole('button', { name: 'Cancel' }));
  await fireEvent.click(within(ask).getByRole('button', { name: 'Cancel' }));

  expect(screen.queryByRole('alertdialog')).toBeNull();
  expect(document.activeElement).toBe(open);
  expect(fetchMock.mock.calls.map((c) => c[1].method)).not.toContain('POST');
});

it('Escape cancels a batch undo; confirming undoes it', async () => {
  routes[PAST] = page([batch()]);
  routes['POST /api/batches/3/undo'] = [200, { batch: batch({ status: 'undone' }), undone: 310, emails: 300, failed: 0 }];
  render(Cleanup);
  const open = await screen.findByRole('button', { name: /^Undo batch/ });
  await fireEvent.click(open);
  await fireEvent.keyDown(screen.getByRole('alertdialog'), { key: 'Escape' });
  expect(screen.queryByRole('alertdialog')).toBeNull();

  await fireEvent.click(open);
  await fireEvent.click(screen.getByRole('button', { name: 'Yes, undo this batch' }));
  await vi.waitFor(() => expect(toast.text).toBe('300 emails moved back where they were'));
  expect(screen.queryByRole('alertdialog')).toBeNull();
});

const startBody = () => {
  const call = fetchMock.mock.calls.find((c) => c[0] === '/api/cleanup/check' && (c[1] as RequestInit).method === 'POST');
  return call ? JSON.parse((call[1] as RequestInit).body as string) : null;
};
const choose = (name: string) => fireEvent.change(screen.getByRole('combobox', { name: 'Which emails' }), { target: { value: name } });
const type = (name: string, value: string) => fireEvent.input(screen.getByRole('spinbutton', { name }), { target: { value } });
const checkButton = () => screen.getByRole<HTMLButtonElement>('button', { name: 'Check what would move' });

it('labels past runs by what they covered, not "all time" for every run without a start', async () => {
  routes[PAST] = page([
    batch({ id: 5, since: null, limit: 25, matched: 4310 }),
    batch({ id: 4, since: null, limit: 2000, matched: 4310 }),
    batch({ id: 3, since: null, limit: 2000, matched: 90 }),
    batch({ id: 2, since: null, limit: null, matched: null }),
  ]);
  render(Cleanup);
  expect(await screen.findByText('me@icloud.com · Inbox · newest 25 emails')).toBeTruthy();
  expect(screen.getByText('me@icloud.com · Inbox · newest 2,000 emails')).toBeTruthy();
  expect(screen.getAllByText('me@icloud.com · Inbox · all time')).toHaveLength(2);
});

it('offers three entries; only the chosen one shows its number box, starting at 200 and 90', async () => {
  render(Cleanup);
  const select = await screen.findByRole<HTMLSelectElement>('combobox', { name: 'Which emails' });
  expect([...select.options].map((o) => o.textContent)).toEqual(['Newest emails', 'From the last days', 'All mail']);
  expect(select.value).toBe('newest');
  expect((screen.getByRole('spinbutton', { name: 'How many emails' }) as HTMLInputElement).value).toBe('200');
  expect(screen.queryByRole('spinbutton', { name: 'How many days' })).toBeNull();

  await choose('days');
  expect(screen.queryByRole('spinbutton', { name: 'How many emails' })).toBeNull();
  expect((screen.getByRole('spinbutton', { name: 'How many days' }) as HTMLInputElement).value).toBe('90');

  await choose('all');
  expect(screen.queryByRole('spinbutton')).toBeNull();
  expect(screen.queryByText('Last 30 days')).toBeNull();
});

it.each([
  ['the newest 120 emails', async () => type('How many emails', '120'), { account_id: 7, folder: 'INBOX', since: null, limit: 120 }],
  ['the newest 5000 is refused before it is sent', async () => type('How many emails', '5000'), null],
  [
    '45 days, with no limit: the daemon covers the most a check takes',
    async () => {
      await choose('days');
      await type('How many days', '45');
    },
    { account_id: 7, folder: 'INBOX', since: NOW - 45 * DAY },
  ],
  [
    'all mail, with no limit: the daemon covers the most a check takes',
    async () => choose('all'),
    { account_id: 7, folder: 'INBOX', since: null },
  ],
] as const)('Check sends %s', async (_name, act, body) => {
  render(Cleanup);
  await screen.findByRole('combobox', { name: 'Which emails' });
  await act();
  if (body === null) {
    expect(checkButton().disabled).toBe(true);
    return;
  }
  await fireEvent.click(checkButton());
  await vi.waitFor(() => expect(startBody()).toEqual(body));
});

it.each([
  ['How many emails', ['0', '2001', '1.5', ''], 'The limit must be between 1 and 2000.', undefined],
  ['How many days', ['0', '2.5', ''], 'Give a whole number of days, 1 or more.', 'days'],
] as const)('the %s box shows its message inline and disables Check while the number is wrong', async (name, bad, message, entry) => {
  render(Cleanup);
  await screen.findByRole('combobox', { name: 'Which emails' });
  if (entry) await choose(entry);
  for (const value of bad) {
    await type(name, value);
    expect((await screen.findByRole('alert')).textContent).toBe(message);
    expect(screen.getByRole('spinbutton', { name }).getAttribute('aria-invalid')).toBe('true');
    expect(checkButton().disabled).toBe(true);
  }
  await type(name, entry ? '1000000' : '2000'); // no upper bound for days; 2000 is the most for emails
  expect(screen.queryByRole('alert')).toBeNull();
  expect(checkButton().disabled).toBe(false);
});

it('says when the range held more than the check covers, while it runs and on the table', async () => {
  const capped = { limit: 2000, total: 2000, matched: 4310 };
  routes[CHECK] = [200, { checks: [check({ status: 'running', done: 120, ...capped, rows: [] })] }];
  render(Cleanup);
  expect(await screen.findByText('Checking the newest 2,000 of 4,310. Run another check for the rest.')).toBeTruthy();

  routes[CHECK] = [200, { checks: [check({ status: 'ready', done: 2000, ...capped })] }];
  dispatch('check.progress', check({ status: 'ready', done: 2000, ...capped, rows: [] }));
  expect(await screen.findByText('Checked the newest 2,000 of 4,310. Run another check for the rest.')).toBeTruthy();
  expect(screen.queryByText(/^Checking the newest/)).toBeNull();
});

it.each([
  ['the range fits in the cap', { limit: 2000, total: 412, matched: 412 }],
  ['a newest-N the user typed, below the cap', { limit: 200, total: 200, matched: 4310 }],
])('says nothing about a cap when %s', async (_name, own) => {
  routes[CHECK] = [200, { checks: [check({ status: 'ready', ...own })] }];
  render(Cleanup);
  await openList();
  await screen.findByRole('checkbox', { name: /^Sort / });
  expect(screen.queryByText(/Run another check for the rest/)).toBeNull();
});

it.each([
  ['a typed 45 days', { since: NOW - 45 * DAY, limit: 2000 }, 'days', '45', 'How many days'],
  ['a typed 120 emails', { since: null, limit: 120 }, 'newest', '120', 'How many emails'],
  ['all mail', { since: null, limit: 2000 }, 'all', null, null],
] as const)('restores %s after a reload, and Check is off until the check is discarded', async (_name, own, entry, value, box) => {
  routes[CHECK] = [200, { checks: [check({ status: 'ready', ...own })] }];
  render(Cleanup);
  await openList();
  await screen.findByRole('checkbox', { name: /^Sort / });
  expect(screen.getByRole<HTMLSelectElement>('combobox', { name: 'Which emails' }).value).toBe(entry);
  if (box) expect((screen.getByRole(`spinbutton`, { name: box }) as HTMLInputElement).value).toBe(value);
  else expect(screen.queryByRole('spinbutton')).toBeNull();
  // The scope on show is the check's own.
  expect(screen.getByRole<HTMLSelectElement>('combobox', { name: 'Which emails' }).disabled).toBe(true);
  if (box) expect(screen.getByRole<HTMLInputElement>('spinbutton', { name: box }).disabled).toBe(true);

  await fireEvent.click(screen.getByRole('button', { name: 'Discard check' }));
  await vi.waitFor(() => expect(screen.getByRole<HTMLSelectElement>('combobox', { name: 'Which emails' }).disabled).toBe(false));
});

// Each bar of the chart as [name, count].
const bars = () =>
  within(screen.getByRole('list', { name: 'What the check would do' }))
    .getAllByRole('listitem')
    .map((li) => [li.children[0].textContent, li.children[2].textContent]);

it('a ready check shows its chart, and unticking an email moves it from its rule to what stays, in step with Sort', async () => {
  routes[CHECK] = [
    200,
    {
      checks: [
        check({
          rows: [
            checkRow({ index: 0, from: 'a@x.io', subject: 'One', rule_name: 'Newsletters', actions: [{ type: 'move', folder: 'Reading' }] }),
            checkRow({ index: 1, from: 'b@x.io', subject: 'Two', rule_name: 'Newsletters', actions: [{ type: 'move', folder: 'Reading' }] }),
            checkRow({ index: 2, from: 'c@x.io', subject: 'Three', rule_name: 'Scams', actions: [{ type: 'trash' }] }),
            checkRow({ index: 3, from: 'd@x.io', subject: 'Four', selectable: false, review: true, actions: [], reason: 'Waiting in Needs review' }),
            checkRow({ index: 4, from: 'e@x.io', subject: 'Five', selectable: false, actions: [], rule_name: '', rule_id: null, reason: 'No rule matched' }),
          ],
        }),
      ],
    },
  ];
  render(Cleanup);
  await screen.findByRole('list', { name: 'What the check would do' });
  // trash_to_folder is on here, so a trash names where Sort will move it, as the Action column does.
  expect(bars()).toEqual([
    ['Newsletters → Reading', '2'],
    ['Scams → MailRules Trash', '1'],
    ['Left in Inbox', '1'],
    ['Needs review', '1'],
  ]);
  expect(sortButton().textContent).toMatch(/^\s*Sort 3\b/);

  await openList();
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Sort a@x.io · One' }));
  expect(bars()).toEqual([
    ['Newsletters → Reading', '1'],
    ['Scams → MailRules Trash', '1'],
    ['Left in Inbox', '2'],
    ['Needs review', '1'],
  ]);
  expect(sortButton().textContent).toMatch(/^\s*Sort 2\b/);
});

it('the chart names the range the check covered', async () => {
  routes[CHECK] = [200, { checks: [check({ since: NOW - 90 * DAY, limit: 2000, rows: [checkRow({ index: 0 }), checkRow({ index: 1 })] })] }];
  render(Cleanup);
  expect((await screen.findByRole('heading', { level: 2, name: /would be sorted like this/ })).textContent).toContain('the last 90 days');
});

it('Choose emails opens the list under the chart and closes it again; it starts closed', async () => {
  routes[CHECK] = [200, { checks: [check({ rows: three() })] }];
  render(Cleanup);
  const toggle = await screen.findByRole('button', { expanded: false });
  expect(screen.queryByRole('checkbox')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Select all' })).toBeNull();

  await fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  expect(screen.getAllByRole('checkbox')).toHaveLength(3);
  expect(document.getElementById(toggle.getAttribute('aria-controls')!)?.contains(screen.getByRole('table'))).toBe(true);

  await fireEvent.click(toggle);
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  expect(screen.queryByRole('checkbox')).toBeNull();
  // Closing the list changes no tick: Sort still counts every ticked row.
  expect(sortButton().textContent).toMatch(/^\s*Sort 3\b/);
});

// MAI-43: the mailbox and rule pickers of a manual run.
const MAILBOXES = [
  { id: 7, label: 'me@icloud.com', status: 'live' },
  { id: 8, label: 'work@fastmail.com', status: 'live' },
  { id: 9, label: 'old@example.org', status: 'auth_failed' },
] as never[];
const RULES = [
  { id: 5, name: 'Newsletters', enabled: true },
  { id: 6, name: 'Retired', enabled: false },
  { id: 11, name: 'Receipts', enabled: true },
] as never[];
const startBodies = () => fetchMock.mock.calls.filter((c) => c[0] === '/api/cleanup/check' && (c[1] as RequestInit).method === 'POST').map((c) => JSON.parse((c[1] as RequestInit).body as string));
const box = (name: string) => screen.getByRole<HTMLInputElement>('checkbox', { name });
const twoChecks = (over: Partial<CleanupCheck> = {}) => [
  check({ account_id: 7, id: 'c7', rows: [checkRow({ index: 0 }), checkRow({ index: 1, uid: 101, subject: 'Second' })], ...over }),
  check({ account_id: 8, id: 'c8', rows: [checkRow({ index: 0, from: 'boss@work.io', subject: 'From work', rule_name: 'Receipts', uid: 7 })], ...over }),
];

it('with one mailbox and one rule there is nothing to pick: neither picker is shown', async () => {
  render(Cleanup);
  expect(await screen.findByRole('button', { name: 'Check what would move' })).toBeTruthy();
  expect(screen.queryByRole('group', { name: 'Mailboxes' })).toBeNull();
  expect(screen.queryByRole('group', { name: 'Rules' })).toBeNull();
});

it('offers the mailboxes, starts on the first, and cannot tick one that is not connected', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  routes['GET /api/accounts/8/folders'] = [200, { items: [] }];
  render(Cleanup);
  const group = await screen.findByRole('group', { name: 'Mailboxes' });
  expect(within(group).getAllByRole('checkbox').map((c) => [(c.closest('label') as HTMLElement).textContent!.trim().replace(/\s+/g, ' '), (c as HTMLInputElement).checked, (c as HTMLInputElement).disabled])).toEqual([
    ['All mailboxes', false, false],
    ['me@icloud.com', true, false],
    ['work@fastmail.com', false, false],
    ['old@example.org · Sign-in failed', false, true],
  ]);
});

it('checks the picked mailboxes together in one request, and every rule when none are picked', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  routes['GET /api/accounts/8/folders'] = [200, { items: [] }];
  routes['POST /api/cleanup/check'] = [202, { checks: twoChecks({ status: 'running', done: 0, total: 3, rows: [] }) }];
  render(Cleanup);
  await fireEvent.click(await screen.findByRole('checkbox', { name: 'work@fastmail.com' }));
  expect(box('me@icloud.com').checked && box('work@fastmail.com').checked).toBe(true);
  expect(screen.getByText('With several mailboxes, mail is taken from each Inbox.')).toBeTruthy();

  await fireEvent.click(checkButton());
  await vi.waitFor(() => expect(startBodies()).toEqual([{ account_ids: [7, 8], folder: 'INBOX', since: null, limit: 200 }]));
  // A line per mailbox while they are checked, and the pickers locked.
  const lines = await screen.findByRole('list', { name: 'Checking each mailbox' });
  expect(within(lines).getAllByRole('listitem').map((li) => li.textContent)).toEqual([
    expect.stringContaining('me@icloud.com'),
    expect.stringContaining('work@fastmail.com'),
  ]);
  expect(box('me@icloud.com').closest('fieldset')!.disabled).toBe(true);
});

it('"All mailboxes" ticks every connected mailbox and unticks them again', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  routes['GET /api/accounts/8/folders'] = [200, { items: [] }];
  render(Cleanup);
  await fireEvent.click(await screen.findByRole('checkbox', { name: 'All mailboxes' }));
  expect([box('me@icloud.com').checked, box('work@fastmail.com').checked, box('old@example.org · Sign-in failed').checked]).toEqual([true, true, false]);
  expect(box('All mailboxes').checked).toBe(true);
  await fireEvent.click(box('All mailboxes'));
  expect(box('me@icloud.com').checked || box('work@fastmail.com').checked).toBe(false);
  expect(checkButton().disabled).toBe(true); // no mailbox, nothing to check
});

it('picks only some rules: they are sent in their usual order, and none picked refuses the check with the reason', async () => {
  Object.assign(rules, { list: RULES, loaded: true });
  render(Cleanup);
  const group = await screen.findByRole('group', { name: 'Rules' });
  // Only the rules that are switched on are offered.
  expect(within(group).queryByRole('checkbox')).toBeNull();
  await fireEvent.click(within(group).getByRole('radio', { name: 'Only some rules' }));
  const picks = within(within(group).getByRole('list', { name: 'Rules to check' })).getAllByRole('checkbox') as HTMLInputElement[];
  expect(picks.map((c) => [c.closest('label')!.textContent!.trim(), c.checked])).toEqual([['Newsletters', true], ['Receipts', true]]);
  expect(group.textContent).toContain('Sender rules, your own');

  await fireEvent.click(picks[0]);
  await fireEvent.click(checkButton());
  await vi.waitFor(() => expect(startBodies()).toEqual([{ account_id: 7, folder: 'INBOX', since: null, limit: 200, rule_ids: [11] }]));
});

it('no rule picked is refused with a sentence and the Check button waits', async () => {
  Object.assign(rules, { list: RULES, loaded: true });
  render(Cleanup);
  const group = await screen.findByRole('group', { name: 'Rules' });
  await fireEvent.click(within(group).getByRole('radio', { name: 'Only some rules' }));
  for (const c of within(group).getAllByRole('checkbox') as HTMLInputElement[]) if (c.checked) await fireEvent.click(c);
  expect((await screen.findByText('Pick at least one rule.')).getAttribute('role')).toBe('alert');
  expect(checkButton().disabled).toBe(true);

  await fireEvent.click(within(group).getByRole('radio', { name: 'Every rule' }));
  expect(screen.queryByText('Pick at least one rule.')).toBeNull();
  expect(checkButton().disabled).toBe(false);
});

it('restores a run over two mailboxes: a line each, the chart of the one shown, Show switches it, Sort takes both', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  routes[CHECK] = [200, { checks: twoChecks() }];
  routes['POST /api/cleanup/run'] = [202, { batches: [batch({ id: 21, run_id: 21, total: 2, status: 'running', done: 0 }), batch({ id: 22, run_id: 21, account_id: 8, total: 1, status: 'running', done: 0 })] }];
  render(Cleanup);

  const lines = await screen.findByRole('list', { name: 'Mailboxes in this run' });
  expect(within(lines).getAllByRole('listitem').map((li) => li.textContent!.replace(/\s+/g, ' ').trim())).toEqual([
    expect.stringContaining('me@icloud.com · 2 emails would be sorted'),
    expect.stringContaining('work@fastmail.com · 1 email would be sorted'),
  ]);
  expect(screen.getByRole('heading', { level: 2, name: /would be sorted like this/ }).textContent).toContain('me@icloud.com: ');
  expect(screen.getByRole('button', { name: 'Shown below' })).toBeTruthy();
  // The pickers are locked while checks are on show.
  expect(box('me@icloud.com').closest('fieldset')!.disabled).toBe(true);
  expect(sortButton().textContent).toMatch(/^\s*Sort 3 emails/);

  await fireEvent.click(screen.getByRole('button', { name: 'Show' }));
  expect(screen.getByRole('heading', { level: 2, name: /would be sorted like this/ }).textContent).toContain('work@fastmail.com: ');
  await openList();
  expect(await screen.findByText('From work')).toBeTruthy();
  expect(screen.queryByText('Second')).toBeNull();

  await fireEvent.click(sortButton());
  await vi.waitFor(() => expect(runBody()).toEqual({ runs: [{ account_id: 7, check_id: 'c7', exclude: [] }, { account_id: 8, check_id: 'c8', exclude: [] }] }));
  // One bar for each mailbox while it sorts.
  const bars = await screen.findAllByRole('progressbar', { name: 'Cleanup progress' });
  expect(bars).toHaveLength(2);

  dispatch('batch.progress', batch({ id: 21, run_id: 21, total: 2, status: 'done', done: 2, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } }));
  dispatch('batch.progress', batch({ id: 22, run_id: 21, account_id: 8, total: 1, status: 'done', done: 1, actions: { done: 1, dry_run: 0, failed: 0, undone: 0 } }));
  await vi.waitFor(() => expect(toast.text).toBe('Cleanup done: 3 emails sorted in 2 mailboxes. Undo it per mailbox, or the whole run, below.'));
});

it('a failed mailbox shows its reason on its own line and cannot be shown, the other can still be sorted', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  routes[CHECK] = [
    200,
    { checks: [check({ account_id: 7, id: 'c7', status: 'failed', error: 'The mail server did not answer.', rows: [] }), twoChecks()[1]] },
  ];
  render(Cleanup);
  const lines = await screen.findByRole('list', { name: 'Mailboxes in this run' });
  expect(within(lines).getByText(/The check failed. The mail server did not answer./)).toBeTruthy();
  expect(within(lines).getAllByRole('button').map((b) => (b as HTMLButtonElement).disabled)).toEqual([true, false]);
  expect(sortButton().textContent).toMatch(/^\s*Sort 1 email\b/);
});

it('a run over several mailboxes is one card in the history, with a line and an Undo for each, and one for the whole run', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  const done = { status: 'done', done: 2, total: 2, run_id: 30, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } } as const;
  const a = batch({ id: 30, account_id: 7, ...done });
  const b = batch({ id: 31, account_id: 8, ...done });
  routes[PAST] = page([b, a, batch({ id: 20, run_id: null })]);
  routes['POST /api/batches/30/undo'] = [200, { batch: { ...a, status: 'undone' }, undone: 2, emails: 2, failed: 0 }];
  routes['POST /api/batches/31/undo'] = [200, { batch: { ...b, status: 'undone' }, undone: 2, emails: 2, failed: 0 }];
  render(Cleanup);

  const run = await screen.findByRole('group', { name: /^Run over 2 mailboxes/ });
  expect(within(run).getByText('me@icloud.com')).toBeTruthy();
  expect(within(run).getByText('work@fastmail.com')).toBeTruthy();
  expect(within(run).getAllByRole('button', { name: /^Undo batch/ })).toHaveLength(2);
  // The batch of one mailbox is a card of its own.
  expect(screen.getAllByRole('button', { name: /^Undo batch/ })).toHaveLength(3);

  await fireEvent.click(within(run).getByRole('button', { name: 'Undo the whole run' }));
  expect(screen.getByRole('alertdialog').textContent).toContain('Move 4 emails back to where they were, in 2 mailboxes?');
  await fireEvent.click(screen.getByRole('button', { name: 'Yes, undo this run' }));
  await vi.waitFor(() => expect(toast.text).toBe('4 emails moved back where they were'));
  expect(within(run).queryByRole('button', { name: 'Undo the whole run' })).toBeNull();
  expect(within(run).getAllByText('Undone')).toHaveLength(2);
});

it('one mailbox of a run can be undone on its own', async () => {
  Object.assign(accounts, { list: MAILBOXES, loaded: true });
  const done = { status: 'done', done: 2, total: 2, run_id: 30, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } } as const;
  const a = batch({ id: 30, account_id: 7, ...done });
  const b = batch({ id: 31, account_id: 8, ...done });
  routes[PAST] = page([b, a]);
  routes['POST /api/batches/31/undo'] = [200, { batch: { ...b, status: 'undone' }, undone: 2, emails: 2, failed: 0 }];
  render(Cleanup);
  const run = await screen.findByRole('group', { name: /^Run over 2 mailboxes/ });
  await fireEvent.click(within(run).getByRole('button', { name: /^Undo batch work@fastmail.com/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Yes, undo this batch' }));
  await vi.waitFor(() => expect(toast.text).toBe('2 emails moved back where they were'));
  expect(calledUndo('/api/batches/30/undo')).toBe(false);
  expect(within(run).getByRole('button', { name: 'Undo the whole run' })).toBeTruthy(); // the other mailbox can still go back
});
const calledUndo = (path: string) => fetchMock.mock.calls.some((c) => c[0] === path);
