import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Batch, CleanupCheck, CleanupCheckRow, UndoResult } from '../api/cleanup';

// Fresh modules per test: the state and the poll timers live in module scope.
let m: typeof import('./cleanup.svelte');
let events: typeof import('../api/events');
let toast: { text: string };

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
  reason: '',
  ...over,
});
const check = (over: Partial<CleanupCheck> = {}): CleanupCheck => ({
  id: 'chk1',
  account_id: 7,
  folder: 'INBOX',
  since: null,
  limit: null,
  status: 'running',
  done: 0,
  total: 412,
  model_calls: 0,
  tokens: 0,
  cost_usd: 0,
  error: '',
  rows: [],
  ...over,
});
// Two selectable rows and one that must wait in Needs review.
const readyCheck = check({
  status: 'ready',
  done: 412,
  model_calls: 80,
  cost_usd: 0.01,
  rows: [row({ index: 0, uid: 1 }), row({ index: 1, uid: 2 }), row({ index: 2, uid: 3, selectable: false, actions: [], rule_name: '', rule_id: null, confidence: null, reason: 'Needs review' })],
});

const CHECK = 'GET /api/cleanup/check?account_id=7';
const failed = (status: number, code: string, message: string): Reply => [status, { error: { code, message } }];

beforeEach(async () => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW * 1000);
  routes = {
    'GET /api/accounts/7/folders': [200, { items: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Old Mail', delimiter: '/', special_use: '\\Archive' }] }],
    [CHECK]: [200, { check: null }],
    'POST /api/cleanup/check': [202, { check: check() }],
    'DELETE /api/cleanup/check?account_id=7': [204],
    'POST /api/cleanup/run': [202, { batch: batch() }],
    'GET /api/batches/3': [200, { batch: batch({ done: 29 }) }],
    'POST /api/batches/3/undo': [200, { batch: { ...finished, status: 'undone' }, undone: 310, failed: 0 } satisfies UndoResult],
  };
  fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.resetModules();
  m = await import('./cleanup.svelte');
  events = await import('../api/events');
  toast = (await import('./toast.svelte')).toast;
  m.setScope({ accountId: '7', range: '30' });
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
  routes[CHECK] = [200, { check: c }];
  m.setScope({ range: '30' });
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
  expect(m.cleanup.batch).toBeNull();
  expect(sent('/api/cleanup/run')).toEqual([]);
});

it.each([
  [{ range: '30' }, { account_id: 7, folder: 'INBOX', since: NOW - 30 * 86400 }],
  [{ range: '365', folder: 'Old Mail' }, { account_id: 7, folder: 'Old Mail', since: NOW - 365 * 86400 }],
  [{ range: 'all' }, { account_id: 7, folder: 'INBOX', since: null }],
] as const)('check for scope %j asks for %j', async (scope, request) => {
  m.setScope(scope);
  await flush();
  await m.check();
  expect(sent('/api/cleanup/check')).toEqual([request]);
  expect(m.cleanup.phase).toBe('checking');
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
  expect(m.cleanup.check).toMatchObject({ done: 50, model_calls: 10 });
  events.dispatch('check.progress', check({ account_id: 99, status: 'running', done: 300 }));
  expect(m.cleanup.check!.done).toBe(50);
});

it('checking → ready loads the rows after the event, which carries none', async () => {
  await m.check();
  routes[CHECK] = [200, { check: readyCheck }];
  events.dispatch('check.progress', check({ status: 'ready' }));
  await flush();
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.check!.rows).toHaveLength(3);
});

it('checking → ready by the poll when the event is missed', async () => {
  await m.check();
  routes[CHECK] = [200, { check: readyCheck }];
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.check!.rows).toHaveLength(3);
});

it('checking → failed shows the error', async () => {
  await m.check();
  events.dispatch('check.progress', check({ status: 'failed', error: 'The mail server did not answer.' }));
  expect(m.cleanup.phase).toBe('failed');
  expect(m.cleanup.check!.error).toBe('The mail server did not answer.');
});

it('restores the mailbox\'s current check on setScope, folder and range and all', async () => {
  routes[CHECK] = [200, { check: check({ status: 'ready', folder: 'Old Mail', since: NOW - 90 * 86400, rows: [row()] }) }];
  m.setScope({ range: '30' });
  await flush();
  expect(m.cleanup.phase).toBe('ready');
  expect(m.cleanup.scope.folder).toBe('Old Mail');
  expect(m.cleanup.scope.range).toBe('90');
  expect(m.cleanup.check!.rows).toHaveLength(1);
});

it('restores a running check and resumes its poll', async () => {
  routes[CHECK] = [200, { check: check({ status: 'running', done: 12 }) }];
  m.setScope({ range: '30' });
  await flush();
  expect(m.cleanup.phase).toBe('checking');
  routes[CHECK] = [200, { check: readyCheck }];
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

it('a check being run refuses a new scope', async () => {
  await m.check();
  m.setScope({ range: 'all' });
  expect(m.cleanup.scope.range).toBe('30');
  expect(m.cleanup.phase).toBe('checking');
});

it('check → ready → sort → done, by polling the batch', async () => {
  await toReady();
  expect(await m.sort()).toBe(true);
  expect(sent('/api/cleanup/run')).toEqual([{ account_id: 7, check_id: 'chk1', exclude: [] }]);
  expect(m.cleanup.phase).toBe('sorting');
  expect(m.cleanup.check).toBeNull();
  expect(m.cleanup.batch).toMatchObject({ status: 'running', done: 0, total: 412 });

  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.batch!.done).toBe(29);

  await finish();
  expect(m.cleanup.phase).toBe('done');
  expect(m.cleanup.batch).toEqual(finished);
  expect(toast.text).toBe('Cleanup done: 412 emails sorted. Undo it as one batch below.');
});

it('sort sends the unticked selectable rows as exclude', async () => {
  await toReady();
  m.toggleRow(1);
  await m.sort();
  expect(sent('/api/cleanup/run')).toEqual([{ account_id: 7, check_id: 'chk1', exclude: [1] }]);
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
  expect(m.cleanup.batch!.done).toBe(0);

  events.dispatch('batch.progress', batch({ done: 200 }));
  expect(m.cleanup).toMatchObject({ phase: 'sorting', batch: { done: 200 } });

  events.dispatch('batch.progress', finished);
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished });

  events.dispatch('batch.progress', batch({ done: 1 }));
  expect(m.cleanup.batch).toEqual(finished);
});

it('discard throws the check away and returns to idle', async () => {
  await toReady();
  await m.discard();
  expect(called('DELETE', '/api/cleanup/check?account_id=7')).toBe(true);
  expect(m.cleanup.check).toBeNull();
  expect(m.cleanup.phase).toBe('idle');
});

it.each([
  ['sorted', finished, 'Cleanup done: 412 emails sorted. Undo it as one batch below.'],
  ['sorted with skipped', batch({ status: 'done', done: 7, actions: { done: 7, dry_run: 0, failed: 0, undone: 0 }, skipped: 3 }), 'Cleanup done: 7 emails sorted, 3 skipped (no longer in the folder). Undo it as one batch below.'],
  ['dry run', batch({ status: 'done', done: 412, actions: { done: 0, dry_run: 310, failed: 0, undone: 0 } }), 'Dry run: 412 emails checked, nothing moved.'],
  ['dry run with skipped', batch({ status: 'done', done: 5, actions: { done: 0, dry_run: 5, failed: 0, undone: 0 }, skipped: 2 }), 'Dry run: 5 emails checked, nothing moved, 2 skipped (no longer in the folder).'],
  ['failed', batch({ status: 'failed', done: 90 }), 'Cleanup was cut short after 90 emails. What it did can be undone as one batch below.'],
])('outcome: %s', (_name, b, text) => expect(m.outcome(b)).toBe(text));

it('done → idle when the batch is undone', async () => {
  await toReady();
  await m.sort();
  await finish();
  await m.undo(m.cleanup.batch!);
  expect(fetchMock.mock.calls.at(-1)![0]).toBe('/api/batches/3/undo');
  expect(m.cleanup).toMatchObject({ phase: 'idle', batch: { id: 3, status: 'undone' } });
  expect(toast.text).toBe('412 emails moved back where they were');
});

it('an undo that could not put everything back keeps the batch on offer', async () => {
  await toReady();
  await m.sort();
  await finish();
  routes['POST /api/batches/3/undo'] = [200, { batch: finished, undone: 300, failed: 10 } satisfies UndoResult];
  await m.undo(m.cleanup.batch!);
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: { status: 'done' } });
  expect(toast.text).toBe('300 actions undone; 10 could not be');
});

const PAST = 'GET /api/batches?kind=cleanup';
const older = batch({ id: 2, status: 'done', done: 80, actions: { done: 60, dry_run: 0, failed: 0, undone: 0 } });

it('load lists past runs and more follows the cursor', async () => {
  routes[PAST] = [200, { items: [finished], next_cursor: '3' }];
  routes[PAST + '&cursor=3'] = [200, { items: [older], next_cursor: null }];
  await m.load();
  expect(m.cleanup).toMatchObject({ status: 'ready', batches: [finished], next: '3', phase: 'idle', batch: null });
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
  expect(m.cleanup).toMatchObject({ phase: 'sorting', batch: { id: 3, done: 10 } });

  events.dispatch('batch.progress', batch({ done: 200, tokens: 900, cost_usd: 0.002 }));
  expect(m.cleanup.batches[0]).toMatchObject({ done: 200, tokens: 900, cost_usd: 0.002 });

  await finish();
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished, batches: [finished, older] });
});

it('a sort started here joins the list, and undoing an older run leaves this one alone', async () => {
  routes[PAST] = [200, { items: [older], next_cursor: null }];
  await m.load();
  await toReady();
  await m.sort();
  expect(m.cleanup.batches.map((b) => b.id)).toEqual([3, 2]);
  await finish();

  routes['POST /api/batches/2/undo'] = [200, { batch: { ...older, status: 'undone' }, undone: 60, failed: 0 } satisfies UndoResult];
  await m.undo(older);
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished, batches: [finished, { id: 2, status: 'undone' }] });
});

it('a refused undo says why and changes nothing', async () => {
  await toReady();
  await m.sort();
  await finish();
  routes['POST /api/batches/3/undo'] = failed(404, 'not_found', 'No such batch.');
  await m.undo(m.cleanup.batch!);
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished });
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
