import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Batch, Preview, UndoResult } from '../api/cleanup';

// Fresh modules per test: the state and the poll timer live in module scope.
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
  ...over,
});
const finished = batch({ status: 'done', done: 412, actions: { done: 310, dry_run: 0, failed: 0, undone: 0 } });
const previewed: Preview = {
  total: 412,
  groups: [
    { outcome: 'rule', rule_id: 5, rule_name: 'Newsletters', count: 298 },
    { outcome: 'none', rule_id: null, rule_name: '', count: 102 },
    { outcome: 'review', rule_id: null, rule_name: '', count: 12 },
  ],
  estimated_model_calls: 80,
  estimated_cost_usd: 0.01,
};
const failed = (status: number, code: string, message: string): Reply => [status, { error: { code, message } }];
const NOT_BUILT = failed(501, 'not_implemented', 'This part of MailRules is not built yet.');

beforeEach(async () => {
  vi.useFakeTimers();
  vi.setSystemTime(NOW * 1000);
  routes = {
    'GET /api/accounts/7/folders': [200, { items: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Old Mail', delimiter: '/', special_use: '\\Archive' }] }],
    'POST /api/cleanup/preview': [200, previewed],
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

const sent = (path: string) => fetchMock.mock.calls.filter((c) => c[0] === path).map((c) => JSON.parse(c[1].body as string));
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

it('run is refused before a preview', async () => {
  expect(await m.run()).toBe(false);
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.batch).toBeNull();
  expect(sent('/api/cleanup/run')).toEqual([]);
  expect(vi.getTimerCount()).toBe(0);
});

it.each([
  [{ range: '30' }, { account_id: 7, folder: 'INBOX', since: NOW - 30 * 86400 }],
  [{ range: '365', folder: 'Old Mail' }, { account_id: 7, folder: 'Old Mail', since: NOW - 365 * 86400 }],
  [{ range: 'all' }, { account_id: 7, folder: 'INBOX', since: null }],
] as const)('idle → previewed: scope %j asks for %j', async (scope, request) => {
  m.setScope(scope);
  await m.preview();
  expect(sent('/api/cleanup/preview')).toEqual([request]);
  expect(m.cleanup.phase).toBe('previewed');
  expect(m.cleanup.preview).toEqual(previewed);
});

it('previewed → idle when the scope changes, so run is refused again', async () => {
  await m.preview();
  m.setScope({ range: '90' });
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.preview).toBeNull();
  expect(await m.run()).toBe(false);
});

it('a preview that answers after the scope changed is dropped', async () => {
  const asked = m.preview();
  m.setScope({ range: '90' });
  await asked;
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.preview).toBeNull();
});

it('previewed → running → done, by polling the batch', async () => {
  await m.preview();
  // The run must match the preview the user saw, however long they looked at it.
  vi.setSystemTime((NOW + 600) * 1000);
  expect(await m.run()).toBe(true);
  expect(sent('/api/cleanup/run')).toEqual(sent('/api/cleanup/preview'));
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.batch).toMatchObject({ status: 'running', done: 0, total: 412 });

  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.batch!.done).toBe(29);

  await finish();
  expect(m.cleanup.phase).toBe('done');
  expect(m.cleanup.preview).toBeNull();
  expect(m.cleanup.batch).toEqual(finished);
  expect(toast.text).toBe('Cleanup done: 412 emails sorted. Undo it as one batch below.');

  // Only the toast's timer is left; the poll was cleared.
  expect(vi.getTimerCount()).toBe(1);
});

it('batch.progress moves the bar and finishes the run without a poll', async () => {
  await m.preview();
  await m.run();

  events.dispatch('batch.progress', batch({ id: 4, done: 400 }));
  expect(m.cleanup.batch!.done).toBe(0);

  events.dispatch('batch.progress', batch({ done: 200 }));
  expect(m.cleanup).toMatchObject({ phase: 'running', batch: { done: 200 } });

  events.dispatch('batch.progress', finished);
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished });
  expect(vi.getTimerCount()).toBe(1);

  // A late event for a finished run changes nothing.
  events.dispatch('batch.progress', batch({ done: 1 }));
  expect(m.cleanup.batch).toEqual(finished);
});

it('a failed poll leaves the run going', async () => {
  await m.preview();
  await m.run();
  routes['GET /api/batches/3'] = failed(500, 'internal', 'Something went wrong.');
  await vi.advanceTimersByTimeAsync(POLL);
  expect(m.cleanup.phase).toBe('running');
  await finish();
  expect(m.cleanup.phase).toBe('done');
});

it('running refuses a new scope, a new preview and a second run', async () => {
  await m.preview();
  await m.run();
  m.setScope({ range: 'all' });
  await m.preview();
  expect(await m.run()).toBe(false);
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.scope.range).toBe('30');
  expect(sent('/api/cleanup/preview')).toHaveLength(1);
  expect(sent('/api/cleanup/run')).toHaveLength(1);
  await finish();
});

it('done → run is refused until a new preview', async () => {
  await m.preview();
  await m.run();
  await finish();
  expect(await m.run()).toBe(false);
  await m.preview();
  expect(m.cleanup.phase).toBe('previewed');
  expect(await m.run()).toBe(true);
  await finish();
});

it.each([
  ['sorted', finished, 'Cleanup done: 412 emails sorted. Undo it as one batch below.'],
  ['dry run', batch({ status: 'done', done: 412, actions: { done: 0, dry_run: 310, failed: 0, undone: 0 } }), 'Dry run: 412 emails checked, nothing moved.'],
  ['failed', batch({ status: 'failed', done: 90 }), 'Cleanup failed after 90 emails. Undo it as one batch below.'],
])('outcome: %s', (_name, b, text) => expect(m.outcome(b)).toBe(text));

it('done → idle when the batch is undone', async () => {
  await m.preview();
  await m.run();
  await finish();
  await m.undo();
  expect(fetchMock.mock.calls.at(-1)![0]).toBe('/api/batches/3/undo');
  expect(m.cleanup).toMatchObject({ phase: 'idle', batch: { id: 3, status: 'undone' } });
  expect(toast.text).toBe('310 emails moved back where they were');
});

it('an undo that could not put everything back keeps the batch on offer', async () => {
  await m.preview();
  await m.run();
  await finish();
  routes['POST /api/batches/3/undo'] = [200, { batch: finished, undone: 300, failed: 10 } satisfies UndoResult];
  await m.undo();
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: { status: 'done' } });
  expect(toast.text).toBe('300 emails moved back where they were; 10 could not be');
});

it.each([
  ['preview', 'POST /api/cleanup/preview', NOT_BUILT, { notBuilt: true, phase: 'idle' }, ''],
  ['preview', 'POST /api/cleanup/preview', failed(500, 'internal', 'Something went wrong.'), { notBuilt: false, phase: 'idle' }, 'Something went wrong.'],
  ['run', 'POST /api/cleanup/run', NOT_BUILT, { notBuilt: true, phase: 'previewed' }, ''],
  ['run', 'POST /api/cleanup/run', failed(409, 'cleanup_running', 'A cleanup is already running.'), { notBuilt: false, phase: 'previewed' }, 'A cleanup is already running.'],
])('%s answered %o leaves %o', async (_step, route, reply, want, text) => {
  routes[route] = reply;
  await m.preview();
  expect(await m.run()).toBe(false);
  expect(m.cleanup).toMatchObject({ ...want, batch: null });
  expect(toast.text).toBe(text);
  expect(vi.getTimerCount()).toBe(text ? 1 : 0);
});

it('a refused undo says why and changes nothing', async () => {
  await m.preview();
  await m.run();
  await finish();
  routes['POST /api/batches/3/undo'] = failed(404, 'not_found', 'No such batch.');
  await m.undo();
  expect(m.cleanup).toMatchObject({ phase: 'done', batch: finished });
  expect(toast.text).toBe('No such batch.');
});
