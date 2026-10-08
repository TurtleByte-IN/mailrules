import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Batch, CleanupCheck, CleanupCheckRow, UndoResult } from '../api/cleanup';
import { scopeProblem } from '../scope';
import { settings } from './settings.svelte';

// The limits GET /api/settings reports, for this module and for the fresh copy each test imports.
const limits = { test_default: 200, test_max: 2000, check_max: 2000 };

// Fresh modules per test: the state and the poll timers live in module scope.
let m: typeof import('./cleanup.svelte');
let events: typeof import('../api/events');
let toast: { text: string };
let accountList: typeof import('./accounts.svelte').accounts;
let ruleList: typeof import('./rules.svelte').rules;

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
let fetchMock: ReturnType<typeof vi.fn<(url: string, init: RequestInit) => Promise<Response>>>;

const NOW = 1790000000;
const POLL = 2000;

const batch = (over: Partial<Batch> = {}): Batch => ({
  id: 3,
  kind: 'cleanup',
  status: 'running',
  total: 412,
  done: 0,
  created_at: NOW,
  actions: { done: 0, dry_run: 0, failed: 0, undone: 0 },
  account_id: 2,
  folder: 'INBOX',
  since: null,
  tokens: 0,
  cost_usd: 0,
  skipped: 0,
  limit: 2000,
  matched: 412,
  run_id: null,
  ...over,
});
const finished = batch({ status: 'done', done: 412, actions: { done: 310, dry_run: 0, failed: 0, undone: 0 } });

const row = (over: Partial<CleanupCheckRow> = {}): CleanupCheckRow => ({
  index: 0,
  from: 'a@x.com',
  subject: 'Hi',
  received_at: NOW,
  folder: 'INBOX',
  uid: 1,
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
  since: null,
  limit: 2000,
  status: 'running',
  done: 0,
  total: 412,
  matched: 412,
  model_calls: 0,
  tokens: 0,
  cost_usd: 0,
  error: '',
  rows: [],
  exclude: [],
  rule_ids: null,
  ...over,
});
// Two selectable rows and one that must wait in Needs review.
const readyCheck = check({
  status: 'ready',
  done: 412,
  model_calls: 80,
  cost_usd: 0.01,
  rows: [row({ index: 0, uid: 1 }), row({ index: 1, uid: 2 }), row({ index: 2, uid: 3, selectable: false, review: true, actions: [], rule_name: '', rule_id: null, confidence: null, reason: 'Needs review' })],
});

const CHECK = 'GET /api/cleanup/checks';
const failed = (status: number, code: string, message: string): Reply => [status, { error: { code, message } }];

beforeEach(async () => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW * 1000);
  routes = {
    'GET /api/accounts/7/folders': [200, { items: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Old Mail', delimiter: '/', special_use: '\\Archive' }] }],
    'GET /api/batches?kind=cleanup': [200, { items: [], next_cursor: null }],
    [CHECK]: [200, { checks: [] }],
    'POST /api/cleanup/check': [202, { checks: [check()] }],
    'DELETE /api/cleanup/check?account_id=7': [204],
    'PUT /api/cleanup/check/selection': [204],
    'POST /api/cleanup/run': [202, { batches: [batch()] }],
    'GET /api/batches/3': [200, { batch: batch({ done: 29 }) }],
    'POST /api/batches/3/undo': [200, { batch: { ...finished, status: 'undone' }, undone: 310, emails: 300, failed: 0 } satisfies UndoResult],
  };
  fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.resetModules();
  m = await import('./cleanup.svelte');
  Object.assign(settings.value, { limits });
  // The fresh copy cleanup.svelte now reads, imported like the others after resetModules.
  Object.assign((await import('./settings.svelte')).settings.value, { limits });
  events = await import('../api/events');
  toast = (await import('./toast.svelte')).toast;
  // Two connected mailboxes and three rules, the second of them switched off.
  accountList = (await import('./accounts.svelte')).accounts;
  Object.assign(accountList, { list: [{ id: 7, label: 'one@x.test', status: 'live' }, { id: 8, label: 'two@x.test', status: 'live' }] });
  ruleList = (await import('./rules.svelte')).rules;
  Object.assign(ruleList, { list: [{ id: 5, name: 'Newsletters', enabled: true }, { id: 6, name: 'Old', enabled: false }, { id: 9, name: 'Receipts', enabled: true }], loaded: true });
  m.setScope({ accountId: '7' });
  await vi.advanceTimersByTimeAsync(0);
});
afterEach(() => {
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

const flush = () => vi.advanceTimersByTimeAsync(0);
const sent = (path: string) => fetchMock.mock.calls.filter((c) => c[0] === path).map((c) => JSON.parse(c[1].body as string));
const called = (method: string, path: string) => fetchMock.mock.calls.some((c) => c[0] === path && (c[1] as RequestInit).method === method);
// Bring the store to a ready check for the selected mailbox.
const toReady = async (c = readyCheck) => {
  routes[CHECK] = [200, { checks: [c] }];
  await m.load();
  await flush();
};
const finish = async () => {
  routes['GET /api/batches/3'] = [200, { batch: finished }];
  await vi.advanceTimersByTimeAsync(POLL);
};

it('choosing a mailbox loads its folders and starts on the inbox', () => {
  expect(m.cleanup.folders.map((f) => f.name)).toEqual(['INBOX', 'Old Mail']);
  m.setScope({ folder: 'Old Mail' });
  m.setScope({ accountId: '8' });
  expect(m.cleanup.scope.folder).toBe('INBOX');
  expect(m.cleanup.folders).toEqual([]);
});

it('sort is refused before a check', async () => {
  expect(await m.sort()).toBe(false);
  expect(m.cleanup.phase).toBe('idle');
  expect(m.focused()).toBeNull();
  expect(sent('/api/cleanup/run')).toEqual([]);
});

it('starts on the newest 200 emails, with 90 days ready in the other box', () => {
  expect(m.cleanup.scope).toMatchObject({ mode: 'newest', newest: 200, days: 90 });
});

it.each([
  ['the newest 200, as it starts', {}, { account_id: 7, folder: 'INBOX', since: null, limit: 200 }],
  ['the newest 120', { newest: 120 }, { account_id: 7, folder: 'INBOX', since: null, limit: 120 }],
  ['the newest 2000', { newest: 2000 }, { account_id: 7, folder: 'INBOX', since: null, limit: 2000 }],
  // With no limit sent, the daemon covers the most a check takes (its `limits.check_max`).
  ['the last 90 days, with no limit', { mode: 'days' }, { account_id: 7, folder: 'INBOX', since: NOW - 90 * 86400 }],
  ['the last 45 days in another folder', { mode: 'days', days: 45, folder: 'Old Mail' }, { account_id: 7, folder: 'Old Mail', since: NOW - 45 * 86400 }],
  ['a huge number of days is all mail: the start is the epoch, not before it', { mode: 'days', days: 999999 }, { account_id: 7, folder: 'INBOX', since: 0 }],
  ['all mail, with no limit', { mode: 'all' }, { account_id: 7, folder: 'INBOX', since: null }],
  ['all mail ignores what the boxes hold', { mode: 'all', newest: 5, days: 3 }, { account_id: 7, folder: 'INBOX', since: null }],
] as const)('check for %s', async (_name, scope, request) => {
  m.setScope(scope);
  await m.check();
  expect(sent('/api/cleanup/check')).toEqual([request]);
  expect(m.cleanup.phase).toBe('checking');
});

it.each([
  [{ newest: 0 }, 'The limit must be between 1 and 2000.'],
  [{ newest: 2001 }, 'The limit must be between 1 and 2000.'],
  [{ newest: 1.5 }, 'The limit must be between 1 and 2000.'],
  [{ newest: null }, 'The limit must be between 1 and 2000.'],
  [{ newest: 2000 }, ''],
  [{ mode: 'days', days: 0 }, 'Give a whole number of days, 1 or more.'],
  [{ mode: 'days', days: 2.5 }, 'Give a whole number of days, 1 or more.'],
  [{ mode: 'days', days: null }, 'Give a whole number of days, 1 or more.'],
  [{ mode: 'days', days: 1000000 }, ''],
  [{ mode: 'all', newest: 0, days: 0 }, ''],
] as const)('the scope %j is refused with "%s"', async (scope, problem) => {
  m.setScope(scope);
  expect(scopeProblem(m.cleanup.scope)).toBe(problem);
  await m.check();
  expect(sent('/api/cleanup/check')).toHaveLength(problem ? 0 : 1);
  expect(m.cleanup.phase).toBe(problem ? 'idle' : 'checking');
});

it('bounds the newest N by the limit the daemon reports, and asks only for a whole number before it is known (MAI-41)', () => {
  const s = { accountId: '7', folder: 'INBOX', mode: 'newest', newest: 4000, days: 90 } as const;
  Object.assign(settings.value, { limits: { ...limits, check_max: 5000 } });
  expect(scopeProblem(s)).toBe('');
  expect(scopeProblem({ ...s, newest: 5001 })).toBe('The limit must be between 1 and 5000.');
  Object.assign(settings.value, { limits: { test_default: 0, test_max: 0, check_max: 0 } });
  expect(scopeProblem({ ...s, newest: 9000 })).toBe('');
  expect(scopeProblem({ ...s, newest: 0 })).toBe('Give a whole number of emails, 1 or more.');
});

it('a refused check leaves idle and says why', async () => {
  routes['POST /api/cleanup/check'] = failed(409, 'account_offline', 'Mailbox is offline.');
  await m.check();
  expect(m.cleanup.phase).toBe('idle');
  expect(toast.text).toBe('Mailbox is offline.');
});

it('check.progress moves the numbers, and an event for another account is ignored', async () => {
  await m.check();
  events.dispatch('check.progress', check({ status: 'running', done: 50, model_calls: 10, cost_usd: 0.005 }));
  expect(m.focused()!.check).toMatchObject({ done: 50, model_calls: 10 });
  events.dispatch('check.progress', check({ account_id: 99, status: 'running', done: 300 }));
  expect(m.focused()!.check!.done).toBe(50);
});

it('checking → ready loads the rows after the event, which carries none', async () => {
  await m.check();
  routes[CHECK] = [200, { checks: [readyCheck] }];
  events.dispatch('check.progress', check({ status: 'ready' }));
  await flush();
  expect(m.cleanup.phase).toBe('ready');
  expect(m.focused()!.check!.rows).toHaveLength(3);
});

it('checking → ready by the poll when the event is missed', async () => {
  await m.check();
  routes[CHECK] = [200, { checks: [readyCheck] }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('ready');
  expect(m.focused()!.check!.rows).toHaveLength(3);
});

it('checking → failed shows the error', async () => {
  await m.check();
  events.dispatch('check.progress', check({ status: 'failed', error: 'The mail server did not answer.' }));
  expect(m.cleanup.phase).toBe('failed');
  expect(m.focused()!.check!.error).toBe('The mail server did not answer.');
});

it.each([
  ['a typed 45 days', { since: NOW - 45 * 86400, limit: 2000 }, { mode: 'days', days: 45 }],
  ['90 days, checked two hours ago', { since: NOW - 90 * 86400 - 7200, limit: 2000 }, { mode: 'days', days: 90 }],
  ['a typed 120 emails', { since: null, limit: 120 }, { mode: 'newest', newest: 120 }],
  ['the newest 1999', { since: null, limit: 1999 }, { mode: 'newest', newest: 1999 }],
  // The newest 2000 and all mail send the same request, so a check cannot tell them apart. They are equal by design.
  ['the newest 2000, which is all mail', { since: null, limit: 2000 }, { mode: 'all' }],
  ['all mail', { since: null, limit: 2000 }, { mode: 'all' }],
] as const)('restores the choice of %s from the check itself, with no snapping to a preset', async (_name, own, choice) => {
  routes[CHECK] = [200, { checks: [check({ status: 'ready', folder: 'Old Mail', rows: [row()], ...own })] }];
  await m.load();
  await flush();
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.scope.folder).toBe('Old Mail');
  expect(m.cleanup.scope).toMatchObject(choice);
  expect(m.focused()!.check!.rows).toHaveLength(1);
});

it('restoring one entry leaves the number typed in the other as it was', async () => {
  m.setScope({ days: 33 });
  routes[CHECK] = [200, { checks: [check({ status: 'ready', since: null, limit: 120, rows: [row()] })] }];
  await m.load();
  await flush();
  expect(m.cleanup.scope).toMatchObject({ mode: 'newest', newest: 120, days: 33 });
});

it('a check on show keeps its own folder and range: they cannot be changed until it is discarded', async () => {
  await toReady();
  expect(m.cleanup.scope).toMatchObject({ mode: 'all', folder: 'INBOX' }); // the fixture is a check of all mail
  m.setScope({ mode: 'days', folder: 'Old Mail' });
  expect(m.cleanup.scope).toMatchObject({ mode: 'all', folder: 'INBOX' });
  expect(m.cleanup.phase).toBe('ready');
  await m.discard();
  m.setScope({ mode: 'days' });
  expect(m.cleanup.scope.mode).toBe('days');
});

it('restores a running check and resumes its poll', async () => {
  routes[CHECK] = [200, { checks: [check({ status: 'running', done: 12 })] }];
  await m.load();
  await flush();
  expect(m.cleanup.phase).toBe('checking');
  routes[CHECK] = [200, { checks: [readyCheck] }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('ready');
});

it('select all, none and toggle count only the selectable rows, across the whole list', async () => {
  await toReady();
  expect(m.selectableCount()).toBe(2);
  expect(m.selectedCount()).toBe(2);
  m.selectNone();
  expect(m.selectedCount()).toBe(0);
  m.selectAll();
  expect(m.selectedCount()).toBe(2);
  m.toggleRow(1);
  expect(m.selectedCount()).toBe(1);
  m.toggleRow(2); // not selectable: ignored
  expect(m.selectedCount()).toBe(1);
});

const SAVE = '/api/cleanup/check/selection';
const SAVE_WAIT = 300;

it("the daemon's saved ticks come back with the check and are what the table shows", async () => {
  await toReady({ ...readyCheck, exclude: [1] });
  expect([...m.focused()!.excluded]).toEqual([1]);
  expect(m.selectedCount()).toBe(1);
  expect(sent(SAVE)).toEqual([]);
});

it('tick changes are saved once after a short wait, latest list only', async () => {
  await toReady();
  m.toggleRow(0);
  m.toggleRow(1);
  m.toggleRow(0);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT - 1);
  expect(sent(SAVE)).toEqual([]);
  await vi.advanceTimersByTimeAsync(1);
  expect(sent(SAVE)).toEqual([{ account_id: 7, check_id: 'chk1', exclude: [1] }]);
  m.selectNone();
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  m.selectAll();
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE).map((b) => b.exclude)).toEqual([[1], [0, 1], []]);
});

it('a failed save toasts the daemon\'s message, keeps the ticks and retries on the next change', async () => {
  await toReady();
  routes['PUT ' + SAVE] = [409, { error: { code: 'preview_stale', message: 'The rules changed since this check. Run a new check, then sort.' } }];
  m.toggleRow(0);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(toast.text).toBe('The rules changed since this check. Run a new check, then sort.');
  expect([...m.focused()!.excluded]).toEqual([0]);
  await vi.advanceTimersByTimeAsync(10_000);
  expect(sent(SAVE)).toHaveLength(1); // no retry loop of its own

  routes['PUT ' + SAVE] = [204];
  m.toggleRow(1);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE).at(-1)).toEqual({ account_id: 7, check_id: 'chk1', exclude: [0, 1] });
});

it('saves go one at a time, so a slow earlier save cannot land after a newer one', async () => {
  await toReady();
  let release!: () => void;
  fetchMock.mockImplementationOnce(async () => {
    await new Promise<void>((r) => (release = r));
    return new Response(null, { status: 204 });
  });
  m.toggleRow(0);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE)).toHaveLength(1); // the first is still on its way
  m.toggleRow(1);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE)).toHaveLength(1); // the second waits for it
  release();
  await flush();
  expect(sent(SAVE).map((b) => b.exclude)).toEqual([[0], [0, 1]]);
  expect([...m.focused()!.excluded].sort()).toEqual([0, 1]);
});

it('an answer asked for before a tick does not overwrite it, and pending ticks reach the daemon before it is asked', async () => {
  await toReady();
  // A GET of the check that is slow to answer, with the ticks as the daemon had them.
  let release!: () => void;
  const orig = fetchMock.getMockImplementation()!;
  fetchMock.mockImplementation(async (url: string, init: RequestInit) => {
    if (init.method === 'GET' && url === '/api/cleanup/checks') await new Promise<void>((r) => (release = r));
    return orig(url, init);
  });
  const loading = m.load();
  await flush();
  m.toggleRow(1); // ticked off while the answer is on its way
  release();
  await loading;
  expect([...m.focused()!.excluded]).toEqual([1]);
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE)).toEqual([{ account_id: 7, check_id: 'chk1', exclude: [1] }]);

  // Returning to the page with a change that has not been sent yet sends it first.
  fetchMock.mockClear();
  fetchMock.mockImplementation(orig);
  m.toggleRow(0);
  await m.load();
  const order = fetchMock.mock.calls.map((c) => (c[1] as RequestInit).method + ' ' + c[0]);
  expect(order.indexOf('PUT ' + SAVE)).toBeGreaterThanOrEqual(0);
  expect(order.indexOf('PUT ' + SAVE)).toBeLessThan(order.indexOf('GET /api/cleanup/checks'));
});

it('Discard, Sort and a new check drop an unsent save', async () => {
  await toReady();
  m.toggleRow(0);
  await m.discard();
  await vi.advanceTimersByTimeAsync(SAVE_WAIT * 2);
  expect(sent(SAVE)).toEqual([]);

  await toReady();
  m.toggleRow(0);
  await m.sort();
  await vi.advanceTimersByTimeAsync(SAVE_WAIT * 2);
  expect(sent(SAVE)).toEqual([]);
  expect(sent('/api/cleanup/run').at(-1)).toEqual({ runs: [{ account_id: 7, check_id: 'chk1', exclude: [0] }] });
});

const NOON = 1790000000;
it.each([
  ['the newest 25 of a bigger folder', { since: null, limit: 25, matched: 4310 }, 'newest 25 emails'],
  ['the newest 1 of a bigger folder', { since: null, limit: 1, matched: 30 }, 'newest 1 email'],
  ['the newest 25 of a folder that holds exactly 25: all of it', { since: null, limit: 25, matched: 25 }, 'all time'],
  ['all mail that fits in the cap', { since: null, limit: 2000, matched: 1500 }, 'all time'],
  ['all mail the cap cut', { since: null, limit: 2000, matched: 4310 }, 'newest 2,000 emails'],
  ['days that fit in the cap', { since: NOON, limit: 2000, matched: 90 }, 'since ' + new Date(NOON * 1000).toLocaleDateString([], { day: 'numeric', month: 'short' })],
  ['days the cap cut', { since: NOON, limit: 2000, matched: 4310 }, 'since ' + new Date(NOON * 1000).toLocaleDateString([], { day: 'numeric', month: 'short' }) + ' · newest 2,000 emails'],
  ['a batch made before the daemon recorded this, no start: all it can say', { since: null, limit: null, matched: null }, 'all time'],
  ['a batch made before the daemon recorded this, with a start', { since: NOON, limit: null, matched: null }, 'since ' + new Date(NOON * 1000).toLocaleDateString([], { day: 'numeric', month: 'short' })],
] as const)('a run of %s is labelled "%s"', (_name, own, text) => expect(m.coverage(batch({ ...own }))).toBe(text));

it('a check being run refuses a new scope', async () => {
  await m.check();
  m.setScope({ mode: 'all' });
  expect(m.cleanup.scope.mode).toBe('newest');
  expect(m.cleanup.phase).toBe('checking');
});

it('check → ready → sort → done, by polling the batch', async () => {
  await toReady();
  expect(await m.sort()).toBe(true);
  expect(sent('/api/cleanup/run')).toEqual([{ runs: [{ account_id: 7, check_id: 'chk1', exclude: [] }] }]);
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.focused()!.check).toBeNull();
  expect(m.focused()!.batch).toMatchObject({ status: 'running', done: 0, total: 412 });

  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.focused()!.batch!.done).toBe(29);

  await finish();
  expect(m.cleanup.phase).toBe('done');
  expect(m.focused()!.batch).toEqual(finished);
  expect(toast.text).toBe('Cleanup done: 412 emails sorted. Undo it as one batch below.');
});

// Anything that resets the state while a poll runs (a screen's teardown, sign-out) must not leave the
// poll asking for a sort or check that is no longer there, nor throw from it: in the browser that is an
// unhandled rejection every two seconds.
// Let the timers run well past several polls, so a poll that was left running would show.
const settle = async () => {
  await vi.advanceTimersByTimeAsync(POLL * 5);
  await vi.advanceTimersByTimeAsync(0);
};

it.each([
  ['the run is emptied', { phase: 'idle', lanes: [] }],
  ['the run is emptied but the phase stays sorting', { lanes: [] }],
  ['the phase leaves sorting but the batch stays', { phase: 'idle' }],
] as const)('a state reset mid-sort stops the batch poll: %s', async (_name, reset) => {
  await toReady();
  await m.sort();
  expect(m.cleanup.phase).toBe('sorting');
  await vi.advanceTimersByTimeAsync(POLL);
  const polls = () => fetchMock.mock.calls.filter((c) => c[0] === '/api/batches/3').length;
  expect(polls()).toBe(1);

  Object.assign(m.cleanup, reset);
  await settle();

  expect(polls()).toBe(1);
  // A poll that threw on the missing state would be an unhandled rejection, which fails the whole vitest run.
});

it.each([
  ['the run is emptied', { phase: 'idle', lanes: [] }],
  ['the phase leaves checking', { phase: 'idle' }],
  ['the mailboxes are cleared', { lanes: [], mailboxes: [] }],
] as const)('a state reset mid-check stops the check poll: %s', async (_name, reset) => {
  await m.check();
  expect(m.cleanup.phase).toBe('checking');
  const polls = () => fetchMock.mock.calls.filter((c) => (c[1] as RequestInit).method === 'GET' && c[0] === '/api/cleanup/checks').length;
  const before = polls();
  await vi.advanceTimersByTimeAsync(POLL);
  expect(polls()).toBe(before + 1);

  Object.assign(m.cleanup, reset);
  await settle();

  expect(polls()).toBe(before + 1);
  // A poll that threw on the missing state would be an unhandled rejection, which fails the whole vitest run.
});

it('starting a second sort does not leave two batch polls running', async () => {
  await toReady();
  await m.sort();
  events.dispatch('batch.progress', finished);
  expect(m.cleanup.phase).toBe('done');
  await toReady();
  routes['GET /api/batches/3'] = [200, { batch: batch({ done: 5 }) }];
  await m.sort();
  const before = fetchMock.mock.calls.filter((c) => c[0] === '/api/batches/3').length;
  await vi.advanceTimersByTimeAsync(POLL);
  expect(fetchMock.mock.calls.filter((c) => c[0] === '/api/batches/3').length).toBe(before + 1);
});

it('sort sends the unticked selectable rows as exclude', async () => {
  await toReady();
  m.toggleRow(1);
  await m.sort();
  expect(sent('/api/cleanup/run')).toEqual([{ runs: [{ account_id: 7, check_id: 'chk1', exclude: [1] }] }]);
});

it('sort is refused with nothing ticked', async () => {
  await toReady();
  m.selectNone();
  expect(await m.sort()).toBe(false);
  expect(sent('/api/cleanup/run')).toEqual([]);
});

it('a stale check turns the phase stale and says to check again', async () => {
  await toReady();
  routes['POST /api/cleanup/run'] = failed(409, 'preview_stale', 'The check is no longer current.');
  expect(await m.sort()).toBe(false);
  expect(m.cleanup.phase).toBe('stale');
  expect(toast.text).toBe('The rules changed since this check. Run a new check, then sort.');
});

it('another refusal of sort flashes its reason and stays ready', async () => {
  await toReady();
  routes['POST /api/cleanup/run'] = failed(409, 'cleanup_running', 'A cleanup is already running for this account.');
  expect(await m.sort()).toBe(false);
  expect(m.cleanup.phase).toBe('ready');
  expect(toast.text).toBe('A cleanup is already running for this account.');
});

it('batch.progress moves the bar and finishes the sort without a poll', async () => {
  await toReady();
  await m.sort();

  events.dispatch('batch.progress', batch({ id: 4, done: 400 }));
  expect(m.focused()!.batch!.done).toBe(0);

  events.dispatch('batch.progress', batch({ done: 200 }));
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.focused()!.batch!.done).toBe(200);

  events.dispatch('batch.progress', finished);
  expect(m.cleanup.phase).toBe('done');
  expect(m.focused()!.batch).toEqual(finished);

  events.dispatch('batch.progress', batch({ done: 1 }));
  expect(m.focused()!.batch).toEqual(finished);
});

it('discard throws the check away and returns to idle', async () => {
  await toReady();
  await m.discard();
  expect(called('DELETE', '/api/cleanup/check?account_id=7')).toBe(true);
  expect(m.focused()).toBeNull();
  expect(m.cleanup.phase).toBe('idle');
});

it.each([
  ['sorted', finished, 'Cleanup done: 412 emails sorted. Undo it as one batch below.'],
  ['sorted with skipped', batch({ status: 'done', done: 7, actions: { done: 7, dry_run: 0, failed: 0, undone: 0 }, skipped: 3 }), 'Cleanup done: 4 emails sorted, 3 skipped (no longer in the folder). Undo it as one batch below.'],
  ['dry run', batch({ status: 'done', done: 412, actions: { done: 0, dry_run: 310, failed: 0, undone: 0 } }), 'Dry run: 412 emails checked, nothing moved.'],
  ['dry run with skipped', batch({ status: 'done', done: 5, actions: { done: 0, dry_run: 5, failed: 0, undone: 0 }, skipped: 2 }), 'Dry run: 3 emails checked, nothing moved, 2 skipped (no longer in the folder).'],
  ['failed', batch({ status: 'failed', done: 90 }), 'Cleanup was cut short after 90 emails. What it did can be undone as one batch below.'],
  ['failed with skipped', batch({ status: 'failed', done: 90, skipped: 4 }), 'Cleanup was cut short after 86 emails, 4 skipped (no longer in the folder). What it did can be undone as one batch below.'],
])('outcome: %s', (_name, b, text) => expect(m.outcome(b)).toBe(text));

// Asked before a batch undo: a number only when every action of the batch is real and still in effect.
const every = 'Put every email this batch moved back where it was?';
it.each([
  ['all real, skipped left out', batch({ done: 16, skipped: 2, actions: { done: 18, dry_run: 0, failed: 0, undone: 0 } }), 'Move 14 emails back to where they were?'],
  ['one email', batch({ done: 1, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } }), 'Move 1 email back to where it was?'],
  ['some dry-run', batch({ done: 9, actions: { done: 4, dry_run: 5, failed: 0, undone: 0 } }), every],
  ['some failed', batch({ done: 9, actions: { done: 8, dry_run: 0, failed: 1, undone: 0 } }), every],
  ['partly undone already', batch({ done: 9, actions: { done: 3, dry_run: 0, failed: 0, undone: 6 } }), every],
  ['no actions recorded', batch({ done: 14 }), every],
])('undoQuestion: %s', (_name, b, text) => expect(m.undoQuestion(b)).toBe(text));

it('done → idle when the batch is undone', async () => {
  await toReady();
  await m.sort();
  await finish();
  await m.undo(m.focused()!.batch!);
  expect(fetchMock.mock.calls.at(-1)![0]).toBe('/api/batches/3/undo');
  expect(m.cleanup.phase).toBe('idle'); // all of the run is undone, so it is over
  expect(m.cleanup.lanes).toEqual([]);
  expect(m.cleanup.batches[0]).toMatchObject({ id: 3, status: 'undone' });
  expect(toast.text).toBe('300 emails moved back where they were');
});

// MAI-46: the count is the emails the undo put back, from the daemon, never the run's ticked rows (batch.total),
// which include rows passed over or only recorded in dry-run.
it.each([
  ['several emails', { undone: 420, emails: 210, failed: 0 }, '210 emails moved back where they were'],
  ['one email', { undone: 2, emails: 1, failed: 0 }, '1 email moved back where it was'],
  ['nothing had been moved', { undone: 0, emails: 0, failed: 0 }, 'Nothing to undo'],
  ['some could not be undone', { undone: 300, emails: 150, failed: 10 }, '300 actions undone; 10 could not be'],
])('undo toast: %s', (_name, counts, text) => expect(m.undoneText(counts)).toBe(text));

it('an undo that could not put everything back keeps the batch on offer', async () => {
  await toReady();
  await m.sort();
  await finish();
  routes['POST /api/batches/3/undo'] = [200, { batch: finished, undone: 300, emails: 150, failed: 10 } satisfies UndoResult];
  await m.undo(m.focused()!.batch!);
  expect(m.cleanup.phase).toBe('done');
  expect(m.focused()!.batch).toMatchObject({ status: 'done' });
  expect(toast.text).toBe('300 actions undone; 10 could not be');
});

const PAST = 'GET /api/batches?kind=cleanup';
const older = batch({ id: 2, status: 'done', done: 80, actions: { done: 60, dry_run: 0, failed: 0, undone: 0 } });

it('load lists past runs and more follows the cursor', async () => {
  routes[PAST] = [200, { items: [finished], next_cursor: '3' }];
  routes[PAST + '&cursor=3'] = [200, { items: [older], next_cursor: null }];
  await m.load();
  expect(m.cleanup).toMatchObject({ status: 'ready', batches: [finished], next: '3', phase: 'idle', lanes: [] });
  await m.more();
  expect(m.cleanup).toMatchObject({ batches: [finished, older], next: null });
});

it('a failed load gives the error state', async () => {
  routes[PAST] = failed(500, 'internal', 'Something went wrong.');
  await m.load();
  expect(m.cleanup).toMatchObject({ status: 'error', error: 'Something went wrong.', batches: [] });
});

it('a sort still going at load is picked up and followed to its end', async () => {
  routes[PAST] = [200, { items: [batch({ done: 10 }), older], next_cursor: null }];
  await m.load();
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.focused()!.batch).toMatchObject({ id: 3, done: 10 });

  events.dispatch('batch.progress', batch({ done: 200, tokens: 900, cost_usd: 0.002 }));
  expect(m.cleanup.batches[0]).toMatchObject({ done: 200, tokens: 900, cost_usd: 0.002 });

  await finish();
  expect(m.cleanup).toMatchObject({ phase: 'done', batches: [finished, older] });
  expect(m.focused()!.batch).toEqual(finished);
});

it('a sort started here joins the list, and undoing an older run leaves this one alone', async () => {
  routes[PAST] = [200, { items: [older], next_cursor: null }];
  await m.load();
  await toReady();
  await m.sort();
  expect(m.cleanup.batches.map((b) => b.id)).toEqual([3, 2]);
  await finish();

  routes['POST /api/batches/2/undo'] = [200, { batch: { ...older, status: 'undone' }, undone: 60, emails: 60, failed: 0 } satisfies UndoResult];
  await m.undo(older);
  expect(m.cleanup).toMatchObject({ phase: 'done', batches: [finished, { id: 2, status: 'undone' }] });
  expect(m.focused()!.batch).toEqual(finished);
});

it('a refused undo says why and changes nothing', async () => {
  await toReady();
  await m.sort();
  await finish();
  routes['POST /api/batches/3/undo'] = failed(404, 'not_found', 'No such batch.');
  await m.undo(m.focused()!.batch!);
  expect(m.cleanup.phase).toBe('done');
  expect(m.focused()!.batch).toEqual(finished);
  expect(toast.text).toBe('No such batch.');
});

it.each([
  ['29 days old', batch({ status: 'done', created_at: NOW - 29 * 86400, actions: { done: 3, dry_run: 0, failed: 0, undone: 0 } }), ''],
  ['30 days old to the second', batch({ status: 'done', created_at: NOW - 30 * 86400, actions: { done: 3, dry_run: 0, failed: 0, undone: 0 } }), ''],
  ['a second past 30 days', batch({ status: 'done', created_at: NOW - 30 * 86400 - 1, actions: { done: 3, dry_run: 0, failed: 0, undone: 0 } }), 'Too old to undo'],
  ['only dry-run actions', batch({ status: 'done', actions: { done: 0, dry_run: 9, failed: 0, undone: 0 } }), 'Dry run: nothing to undo'],
  ['dry-run and real actions', batch({ status: 'failed', actions: { done: 0, dry_run: 9, failed: 1, undone: 0 } }), ''],
  ['no actions at all', batch({ status: 'done' }), ''],
])('noUndo: %s', (_name, b, why) => expect(m.noUndo(b, NOW * 1000)).toBe(why));

// The chart, from a check's rows and the user's ticks.
const sorted = (over: Partial<CleanupCheckRow>) => row({ selectable: true, ...over });
const waiting = (index: number) => row({ index, selectable: false, review: true, actions: [], rule_name: 'Offers', reason: 'Waiting in Needs review' });
const alone = (index: number) => row({ index, selectable: false, actions: [], rule_name: '', rule_id: null, reason: 'No rule matched' });
const chartRows = [
  sorted({ index: 0, rule_name: 'Newsletters', actions: [{ type: 'move', folder: 'Reading' }, { type: 'read' }] }),
  sorted({ index: 1, rule_name: 'Newsletters', actions: [{ type: 'move', folder: 'Reading' }] }),
  sorted({ index: 2, rule_name: 'Orders', actions: [{ type: 'archive' }] }),
  sorted({ index: 3, rule_name: 'Scams', actions: [{ type: 'trash' }] }),
  sorted({ index: 4, rule_name: 'Sender rule: keep', actions: [{ type: 'keep' }] }),
  waiting(5),
  alone(6),
];
const counts = (bars: { name: string; count: number }[]) => bars.map((b) => [b.name, b.count]);

it('the chart has a row per rule and where it sends mail, rules biggest first and trash after them, then what is left and what waits', () => {
  expect(m.chartRows(chartRows, new Set(), 'MailRules Trash')).toEqual([
    { name: 'Newsletters → Reading', count: 2, kind: 'rule' },
    { name: 'Orders → Archive', count: 1, kind: 'rule' },
    { name: 'Sender rule: keep → Inbox', count: 1, kind: 'rule' },
    { name: 'Scams → MailRules Trash', count: 1, kind: 'trash' },
    { name: 'Left in Inbox', count: 1, kind: 'left' },
    { name: 'Needs review', count: 1, kind: 'review' },
  ]);
  // Without the setting a trash goes to the server's Trash; another folder names itself in what stays.
  expect(counts(m.chartRows(chartRows, new Set(), undefined, 'Archive'))).toContainEqual(['Scams → Trash', 1]);
  expect(counts(m.chartRows(chartRows, new Set(), undefined, 'Archive'))).toContainEqual(['Left in Archive', 1]);
});

it('an unticked row counts as left in the folder, keeps its rule row in place, and the rule rows add up to Sort', () => {
  const bars = m.chartRows(chartRows, new Set([0, 1, 3]));
  expect(counts(bars)).toEqual([
    ['Newsletters → Reading', 0],
    ['Orders → Archive', 1],
    ['Sender rule: keep → Inbox', 1],
    ['Scams → Trash', 0],
    ['Left in Inbox', 4],
    ['Needs review', 1],
  ]);
  const ruleTotal = bars.filter((b) => b.kind === 'rule' || b.kind === 'trash').reduce((a, b) => a + b.count, 0);
  expect(ruleTotal).toBe(2);
  // Every row is counted once, ticked or not.
  expect(bars.reduce((a, b) => a + b.count, 0)).toBe(chartRows.length);
});

it('ticks never move a Needs-review or left-alone row', () => {
  expect(counts(m.chartRows([waiting(0), alone(1)], new Set([0, 1])))).toEqual([
    ['Left in Inbox', 1],
    ['Needs review', 1],
  ]);
});

it('an empty check has nothing but zero rows for what is left and what waits', () => {
  expect(counts(m.chartRows([], new Set()))).toEqual([
    ['Left in Inbox', 0],
    ['Needs review', 0],
  ]);
});

it('the chart follows the ticks the store holds', async () => {
  await toReady();
  const sortable = () => m.chartRows(m.focused()!.check!.rows, m.focused()!.excluded).filter((b) => b.kind === 'rule').reduce((a, b) => a + b.count, 0);
  expect(sortable()).toBe(m.selectedCount());
  m.toggleRow(1);
  expect(sortable()).toBe(1);
  expect(sortable()).toBe(m.selectedCount());
  m.selectNone();
  expect(sortable()).toBe(0);
});

it.each<[string, Partial<CleanupCheck>, string]>([
  ['the newest N', { since: null, limit: 3 }, 'The newest 3 emails in Inbox would be sorted like this'],
  ['the last days', { since: NOW - 90 * 86400, limit: 2000 }, '3 emails in Inbox from the last 90 days would be sorted like this'],
  ['all mail', { since: null, limit: 2000 }, 'All 3 emails in Inbox would be sorted like this'],
  ['the newest 2,000 of a larger range', { since: NOW - 365 * 86400, limit: 2000, total: 3, matched: 4310 }, 'The newest 3 of 4,310 emails in Inbox from the last 365 days would be sorted like this'],
  ['all mail, capped', { since: null, limit: 2000, total: 3, matched: 4310 }, 'The newest 3 of 4,310 emails in Inbox would be sorted like this'],
  ['nothing found', { since: NOW - 7 * 86400, limit: 2000, rows: [] }, 'No emails in Inbox from the last 7 days to sort'],
])('the chart title says what was checked: %s', (_name, own, title) => {
  expect(m.chartTitle(check({ status: 'ready', total: 3, matched: 3, rows: [row({ index: 0 }), row({ index: 1 }), row({ index: 2 })], ...own }))).toBe(title);
});

// MAI-43: a manual run over any mix of mailboxes and rules.
const two = (over: Partial<CleanupCheck> = {}) => [check({ account_id: 7, id: 'c7', ...over }), check({ account_id: 8, id: 'c8', ...over })];
const readyRows = [row({ index: 0, uid: 1 }), row({ index: 1, uid: 2 })];
const CHECK_ONE = 'DELETE /api/cleanup/check?account_id=';
const batchOf = (account: number, id: number, over: Partial<Batch> = {}) => batch({ id, account_id: account, run_id: 20, total: 2, ...over });

it('mailboxes are picked in the order of the mailbox list, and several share only their Inbox', () => {
  m.setScope({ folder: 'INBOX' });
  m.setMailboxes(['8', '7']);
  expect(m.cleanup.mailboxes).toEqual(['7', '8']);
  expect(m.cleanup.scope).toMatchObject({ accountId: '7', folder: 'INBOX' });
  expect(m.cleanup.folders).toEqual([]);
  m.setMailboxes(['8']);
  expect(m.cleanup.mailboxes).toEqual(['8']);
  expect(m.cleanup.scope.accountId).toBe('8'); // the mailbox on show moved to one that is picked
});

it('checks every picked mailbox together with one request, and shows one line per mailbox', async () => {
  routes['POST /api/cleanup/check'] = [202, { checks: two({ status: 'running' }) }];
  m.setMailboxes(['7', '8']);
  await m.check();
  expect(sent('/api/cleanup/check')).toEqual([{ account_ids: [7, 8], folder: 'INBOX', since: null, limit: 200 }]);
  expect(m.cleanup.phase).toBe('checking');
  expect(m.cleanup.lanes.map((l) => [l.accountId, l.check?.status])).toEqual([['7', 'running'], ['8', 'running']]);

  // Progress is per mailbox, and an event for a mailbox that is not in the run is ignored.
  events.dispatch('check.progress', check({ account_id: 8, id: 'c8', status: 'running', done: 50 }));
  events.dispatch('check.progress', check({ account_id: 99, status: 'running', done: 300 }));
  expect(m.laneOf(8)!.check!.done).toBe(50);
  expect(m.laneOf(7)!.check!.done).toBe(0);

  // One mailbox is through: the run is still checking until the other is.
  routes[CHECK] = [200, { checks: [check({ account_id: 7, id: 'c7', status: 'ready', rows: readyRows }), check({ account_id: 8, id: 'c8', status: 'running', done: 90 })] }];
  events.dispatch('check.progress', check({ account_id: 7, id: 'c7', status: 'ready' }));
  await flush();
  expect(m.cleanup.phase).toBe('checking');
  expect(m.laneOf(7)!.check!.rows).toHaveLength(2);
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows }) }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('ready');
  expect(m.selectedCount()).toBe(4);
});

it('one mailbox keeps the original request shape', async () => {
  await m.check();
  expect(sent('/api/cleanup/check')).toEqual([{ account_id: 7, folder: 'INBOX', since: null, limit: 200 }]);
});

it('a failed mailbox does not spoil the others: the run is ready, the failed one has no rows to sort', async () => {
  routes['POST /api/cleanup/check'] = [202, { checks: two({ status: 'running' }) }];
  m.setMailboxes(['7', '8']);
  await m.check();
  routes[CHECK] = [200, { checks: [check({ account_id: 7, id: 'c7', status: 'failed', error: 'The mail server did not answer.' }), check({ account_id: 8, id: 'c8', status: 'ready', rows: readyRows })] }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('ready');
  expect(m.selectedCount()).toBe(2);

});

it('all mailboxes failing is a failed run, and a stale one makes the whole run stale', async () => {
  routes['POST /api/cleanup/check'] = [202, { checks: two({ status: 'running' }) }];
  m.setMailboxes(['7', '8']);
  await m.check();
  routes[CHECK] = [200, { checks: two({ status: 'failed', error: 'The mail server did not answer.' }) }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('failed');

  m.setMailboxes(['7', '8']);
  await m.check();
  routes[CHECK] = [200, { checks: [check({ account_id: 7, id: 'c7', status: 'ready', rows: readyRows }), check({ account_id: 8, id: 'c8', status: 'stale', rows: readyRows })] }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('stale');
  expect(await m.sort()).toBe(false);
});

it('the picked mailboxes and rules cannot change while a run is checking, shown, or sorting', async () => {
  m.setMailboxes(['7', '8']);
  await toReady(readyCheck);
  m.setMailboxes(['7']);
  m.setRules([5]);
  expect(m.cleanup.mailboxes).toEqual(['7']); // the restored run is of the mailbox the check belongs to
  expect(m.cleanup.ruleIds).toBeNull();
  expect(m.cleanup.phase).toBe('ready');
});

describe('rules', () => {
  it('only the picked rules are sent, and a rule switched off or deleted since is dropped', async () => {
    m.setRules([5, 6, 9, 77]);
    expect(m.pickedRules()).toEqual([5, 9]);
    await m.check();
    expect(sent('/api/cleanup/check')).toEqual([{ account_id: 7, folder: 'INBOX', since: null, limit: 200, rule_ids: [5, 9] }]);
  });

  it('every rule leaves rule_ids out', async () => {
    m.setRules(null);
    await m.check();
    expect(sent('/api/cleanup/check')[0]).not.toHaveProperty('rule_ids');
  });

  it.each([
    ['none picked', [], 'Pick at least one rule.'],
    ['only a rule that is switched off', [6], 'Pick at least one rule.'],
    ['one rule', [5], ''],
    ['every rule', null, ''],
  ])('the picked rules are refused when needed: %s', async (_name, ids, problem) => {
    m.setRules(ids);
    expect(m.ruleProblem()).toBe(problem);
    await m.check();
    expect(sent('/api/cleanup/check')).toHaveLength(problem ? 0 : 1);
  });

  it('a restored check brings its rule picks back', async () => {
    await toReady({ ...readyCheck, rule_ids: [5] });
    expect(m.cleanup.ruleIds).toEqual([5]);
  });
});

it('a restored run over two mailboxes brings both back, with the mailbox on show kept if it is one of them', async () => {
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows, exclude: [1] }) }];
  m.setMailboxes(['8']);
  await m.load();
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.mailboxes).toEqual(['7', '8']);
  expect(m.cleanup.scope.accountId).toBe('8');
  expect(m.laneOf(7)!.excluded).toEqual(new Set([1]));
  expect(m.selectedCount()).toBe(2); // one of two rows ticked in each mailbox
  m.focus('7');
  expect(m.focused()!.accountId).toBe('7');
  m.focus('99'); // not in the run
  expect(m.focused()!.accountId).toBe('7');
});

it('ticks are per mailbox: they are saved with each check, and Select all only touches the one on show', async () => {
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows }) }];
  await m.load();
  m.toggleRow(0);
  m.focus('8');
  m.selectNone();
  await vi.advanceTimersByTimeAsync(SAVE_WAIT);
  expect(sent(SAVE)).toEqual([
    { account_id: 7, check_id: 'c7', exclude: [0] },
    { account_id: 8, check_id: 'c8', exclude: [0, 1] },
  ]);
  expect(m.selectedCount(m.laneOf(7))).toBe(1);
  expect(m.selectedCount(m.laneOf(8))).toBe(0);
  expect(m.selectedCount()).toBe(1);
});

it('sort starts every ready mailbox together with its own unticked rows, and is done when the last batch is', async () => {
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows }) }];
  routes['POST /api/cleanup/run'] = [202, { batches: [batchOf(7, 21), batchOf(8, 22)] }];
  routes['GET /api/batches/21'] = [200, { batch: batchOf(7, 21, { done: 1 }) }];
  routes['GET /api/batches/22'] = [200, { batch: batchOf(8, 22, { done: 1 }) }];
  await m.load();
  m.toggleRow(1); // on the mailbox on show: 7
  expect(await m.sort()).toBe(true);
  expect(sent('/api/cleanup/run')).toEqual([
    { runs: [{ account_id: 7, check_id: 'c7', exclude: [1] }, { account_id: 8, check_id: 'c8', exclude: [] }] },
  ]);
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.cleanup.lanes.map((l) => l.batch?.id)).toEqual([21, 22]);
  expect(m.cleanup.batches.map((b) => b.id)).toEqual([21, 22]);

  const done = (account: number, id: number) => batchOf(account, id, { status: 'done', done: 2, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } });
  events.dispatch('batch.progress', done(8, 22));
  expect(m.cleanup.phase).toBe('sorting'); // the other mailbox is still going
  expect(toast.text).toBe('');
  events.dispatch('batch.progress', done(7, 21));
  expect(m.cleanup.phase).toBe('done');
  expect(toast.text).toBe('Cleanup done: 4 emails sorted in 2 mailboxes. Undo it per mailbox, or the whole run, below.');
});

it('a mailbox with nothing ticked or no rows is left out of the run, and its check is let go', async () => {
  routes[CHECK] = [200, { checks: [check({ account_id: 7, id: 'c7', status: 'ready', rows: readyRows }), check({ account_id: 8, id: 'c8', status: 'ready', rows: [] })] }];
  routes['POST /api/cleanup/run'] = [202, { batches: [batchOf(7, 21, { run_id: null })] }];
  routes[CHECK_ONE + '8'] = [204];
  await m.load();
  expect(await m.sort()).toBe(true);
  expect(sent('/api/cleanup/run')).toEqual([{ runs: [{ account_id: 7, check_id: 'c7', exclude: [] }] }]);
  expect(called('DELETE', '/api/cleanup/check?account_id=8')).toBe(true);
  expect(m.cleanup.lanes.map((l) => l.accountId)).toEqual(['7']);
});

it('a run refused whole leaves every check as it was, and says why', async () => {
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows }) }];
  routes['POST /api/cleanup/run'] = failed(409, 'cleanup_running', 'A cleanup is already running for this account. Wait for it to finish.');
  await m.load();
  expect(await m.sort()).toBe(false);
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.lanes.map((l) => l.check?.id)).toEqual(['c7', 'c8']);
  expect(toast.text).toBe('A cleanup is already running for this account. Wait for it to finish.');
});

it('discard throws every mailbox check away', async () => {
  routes[CHECK] = [200, { checks: two({ status: 'ready', rows: readyRows }) }];
  routes[CHECK_ONE + '7'] = [204];
  routes[CHECK_ONE + '8'] = [204];
  await m.load();
  await m.discard();
  expect(called('DELETE', '/api/cleanup/check?account_id=7') && called('DELETE', '/api/cleanup/check?account_id=8')).toBe(true);
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.lanes).toEqual([]);
});

it('a run still going at load is picked up as one lane per mailbox', async () => {
  routes['GET /api/batches?kind=cleanup'] = [200, { items: [batchOf(8, 22, { done: 1 }), batchOf(7, 21, { done: 1 })], next_cursor: null }];
  await m.load();
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.cleanup.lanes.map((l) => l.batch?.id)).toEqual([22, 21]);
});

describe('past runs', () => {
  it('batches started together are one run, in the order the history lists them, one line per mailbox', () => {
    const a = batchOf(7, 21);
    const b = batchOf(8, 22);
    const solo = batch({ id: 19, run_id: null });
    const earlier = batchOf(7, 15, { run_id: 15 });
    expect(m.runs([b, a, solo, earlier]).map((r) => [r.id, r.batches.map((x) => x.id)])).toEqual([
      [20, [21, 22]], // the run takes the place of its newest batch; its lines run in the order they started
      [19, [19]],
      [15, [15]],
    ]);
  });

  it('undoing a whole run asks once, counts every mailbox, and undoes each batch that can be', async () => {
    const done = { status: 'done', done: 2, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } } as const;
    const a = batchOf(7, 21, done);
    const b = batchOf(8, 22, done);
    const old = batchOf(7, 23, { ...done, created_at: NOW - 40 * 86400 });
    expect(m.undoRunQuestion([a, b])).toBe('Move 4 emails back to where they were, in 2 mailboxes?');
    expect(m.undoRunQuestion([a, old])).toBe('Move 2 emails back to where they were, in its mailbox?'); // the old one cannot be undone
    expect(m.undoRunQuestion([batchOf(7, 21, { ...done, actions: { done: 0, dry_run: 2, failed: 0, undone: 0 } }), b])).toBe('Move 2 emails back to where they were, in its mailbox?'); // a dry run has nothing to undo
    expect(m.undoRunQuestion([batchOf(7, 21, { ...done, actions: { done: 1, dry_run: 1, failed: 0, undone: 0 } }), b])).toBe(
      'Put every email this run moved back where it was, in 2 mailboxes?',
    );

    routes['POST /api/batches/21/undo'] = [200, { batch: { ...a, status: 'undone' }, undone: 2, emails: 2, failed: 0 } satisfies UndoResult];
    routes['POST /api/batches/22/undo'] = [200, { batch: { ...b, status: 'undone' }, undone: 2, emails: 2, failed: 0 } satisfies UndoResult];
    m.cleanup.batches = [a, b, old];
    await m.undoRun([a, b, old]);
    expect(called('POST', '/api/batches/21/undo') && called('POST', '/api/batches/22/undo')).toBe(true);
    expect(called('POST', '/api/batches/23/undo')).toBe(false);
    expect(m.cleanup.batches.map((x) => x.status)).toEqual(['undone', 'undone', 'done']);
    expect(toast.text).toBe('4 emails moved back where they were');
  });

  it('a mailbox that cannot be undone does not stop the others, and the first failure is told', async () => {
    const done = { status: 'done', done: 2, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 } } as const;
    const a = batchOf(7, 21, done);
    const b = batchOf(8, 22, done);
    routes['POST /api/batches/21/undo'] = failed(409, 'too_old', 'This was done more than 30 days ago, so it can no longer be undone.');
    routes['POST /api/batches/22/undo'] = [200, { batch: { ...b, status: 'undone' }, undone: 2, emails: 2, failed: 0 } satisfies UndoResult];
    m.cleanup.batches = [a, b];
    await m.undoRun([a, b]);
    expect(m.cleanup.batches.map((x) => x.status)).toEqual(['done', 'undone']);
    expect(toast.text).toBe('This was done more than 30 days ago, so it can no longer be undone.');
  });

  it.each([
    ['all sorted', [{}, {}], 'Cleanup done: 4 emails sorted in 2 mailboxes. Undo it per mailbox, or the whole run, below.'],
    ['some skipped', [{ skipped: 1 }, {}], 'Cleanup done: 3 emails sorted in 2 mailboxes, 1 skipped (no longer in the folder). Undo it per mailbox, or the whole run, below.'],
    ['dry run', [{ actions: { done: 0, dry_run: 2, failed: 0, undone: 0 } }, { actions: { done: 0, dry_run: 2, failed: 0, undone: 0 } }], 'Dry run: 4 emails checked in 2 mailboxes, nothing moved.'],
    ['one cut short', [{ status: 'failed', done: 1 }, {}], 'Cleanup was cut short on 1 of 2 mailboxes after 3 emails. What it did can be undone below.'],
  ] as const)('runOutcome: %s', (_name, over, text) => {
    const mk = (account: number, i: number) => batchOf(account, 21 + i, { status: 'done', done: 2, actions: { done: 2, dry_run: 0, failed: 0, undone: 0 }, ...over[i] });
    expect(m.runOutcome([mk(7, 0), mk(8, 1)])).toBe(text);
  });
});
