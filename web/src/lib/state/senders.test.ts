import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Sender } from '../api/senders';

// Fresh modules per test: the state lives in module scope.
let m: typeof import('./senders.svelte');
let toast: { text: string };

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
let fetchMock: ReturnType<typeof vi.fn<(url: string, init: RequestInit) => Promise<Response>>>;

const sender = (value: string, over: Partial<Sender> = {}): Sender => ({
  type: 'address',
  value,
  name: '',
  messages: 1,
  last_seen_at: 1790000000,
  has_list_unsubscribe: false,
  verdict: null,
  rule_id: null,
  folder: null,
  source: null,
  hits: 0,
  ...over,
});

const swiggy = sender('a+b@swiggy.in', { name: 'Swiggy', messages: 64 });
const hdfc = sender('alerts@hdfcbank.net', { name: 'HDFC Bank', verdict: 'keep', source: 'user', hits: 4 });
const jobs = sender('jobalerts.in', { type: 'domain', verdict: 'route', rule_id: 5, source: 'learned', hits: 3 });
const SWIGGY = '/api/senders/address/a%2Bb%40swiggy.in';
const LIST = 'GET /api/senders?sort=volume';
const failed = (status: number, code: string, message: string): Reply => [status, { error: { code, message } }];

beforeEach(async () => {
  routes = {
    [LIST]: [200, { items: [swiggy, hdfc, jobs], next_cursor: 'c2' }],
    'GET /api/senders?source=learned&limit=100': [200, { items: [jobs], next_cursor: null }],
  };
  fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.resetModules();
  m = await import('./senders.svelte');
  toast = (await import('./toast.svelte')).toast;
  // Only id and name are read, so this holds before and after rules move to the contract's Rule.
  Object.assign((await import('./rules.svelte')).rules, { list: [{ id: 5, name: 'Recruiters' }] });
});
afterEach(() => vi.unstubAllGlobals());

const row = (s: Sender) => m.senders.list.find((x) => x.value === s.value)!;
const lastCall = () => fetchMock.mock.calls.at(-1)!;

it('load fills the list, its cursor and the learned rules', async () => {
  await m.load();
  expect(m.senders).toMatchObject({ status: 'ready', list: [swiggy, hdfc, jobs], next: 'c2', learned: [jobs] });
});

it('a failed load gives the error state', async () => {
  routes[LIST] = failed(500, 'internal', 'Something went wrong.');
  await m.load();
  expect(m.senders).toMatchObject({ status: 'error', error: 'Something went wrong.', list: [] });
});

it('sort and search are asked of the server', async () => {
  routes['GET /api/senders?sort=recent'] = [200, { items: [hdfc], next_cursor: null }];
  await m.setSort('recent');
  expect(m.senders).toMatchObject({ list: [hdfc], next: null });

  routes['GET /api/senders?sort=recent&q=swiggy'] = [200, { items: [swiggy], next_cursor: null }];
  await m.setQuery('  swiggy ');
  expect(m.senders.list).toEqual([swiggy]);
});

it('a slow answer to an earlier search does not replace a later one', async () => {
  const page = (items: Sender[]) => new Response(JSON.stringify({ items, next_cursor: null }));
  let answerFirst = (_r: Response) => {};
  fetchMock.mockImplementationOnce(() => new Promise((resolve) => (answerFirst = resolve)));
  fetchMock.mockImplementationOnce(async () => page([swiggy]));

  const first = m.setQuery('s');
  await m.setQuery('sw');
  answerFirst(page([hdfc]));
  await first;
  expect(m.senders.list).toEqual([swiggy]);
});

it('more appends the next page', async () => {
  await m.load();
  routes[LIST + '&cursor=c2'] = [200, { items: [sender('z@z.dev')], next_cursor: null }];
  await m.more();
  expect(m.senders.list.map((s) => s.value)).toEqual([swiggy.value, hdfc.value, jobs.value, 'z@z.dev']);
  expect(m.senders.next).toBeNull();
});

it.each([
  [swiggy, 'auto', 'a+b@swiggy.in'],
  [hdfc, 'keep', 'Keep in Inbox'],
  [sender('x@y.z', { verdict: 'block' }), 'trash', 'Trash'],
  [jobs, '5', 'Recruiters'],
  [sender('x@y.z', { verdict: 'route', rule_id: 99 }), '99', '99'],
  [sender('x@y.z', { verdict: 'move', folder: 'Work/Clients' }), 'move:Work/Clients', 'Move to Work/Clients'],
] as const)('%o routes as %s', (s, routing, target) => {
  expect(m.routingOf(s)).toBe(routing);
  if (routing !== 'auto') expect(m.targetOf(s)).toBe(target);
});

it.each([
  ['keep', { verdict: 'keep' }, 'Mail from Swiggy: Keep in Inbox'],
  ['trash', { verdict: 'block' }, 'Mail from Swiggy: Trash'],
  ['5', { verdict: 'route', rule_id: 5 }, 'Mail from Swiggy: Recruiters'],
  ['move:Receipts', { verdict: 'move', folder: 'Receipts' }, 'Mail from Swiggy: Move to Receipts'],
] as const)('routing %s puts %o', async (routing, body, text) => {
  await m.load();
  const saved = { ...swiggy, ...body, rule_id: 'rule_id' in body ? body.rule_id : null, folder: 'folder' in body ? body.folder : null, source: 'user' as const };
  routes['PUT ' + SWIGGY] = [200, { sender: saved }];

  expect(await m.setRouting(row(swiggy), routing)).toBe(true);
  expect(JSON.parse(lastCall()[1].body as string)).toEqual(body);
  expect(m.routingOf(row(swiggy))).toBe(routing);
  expect(toast.text).toBe(text);
});

it('routing auto deletes the sender rule', async () => {
  await m.load();
  routes['DELETE /api/senders/address/alerts%40hdfcbank.net'] = [204];
  expect(await m.setRouting(row(hdfc), 'auto')).toBe(true);
  expect(lastCall()[1].method).toBe('DELETE');
  expect(row(hdfc)).toMatchObject({ verdict: null, rule_id: null, folder: null, source: null, hits: 0 });
  expect(toast.text).toBe('HDFC Bank goes back to your rules');
});

it('a refused routing changes nothing and says why', async () => {
  await m.load();
  routes['PUT ' + SWIGGY] = failed(400, 'invalid_input', 'That rule no longer exists.');
  expect(await m.setRouting(row(swiggy), '5')).toBe(false);
  expect(m.routingOf(row(swiggy))).toBe('auto');
  expect(toast.text).toBe('That rule no longer exists.');
});

it('forget deletes a learned rule; a sender with no name is called by its value', async () => {
  await m.load();
  routes['DELETE /api/senders/domain/jobalerts.in'] = [204];
  await m.forget(m.senders.learned[0]);
  expect(m.senders.learned).toEqual([]);
  expect(m.routingOf(row(jobs))).toBe('auto');
  expect(m.nameOf(jobs)).toBe('jobalerts.in');
});

it('setting a learned sender by hand takes it off the learned list', async () => {
  await m.load();
  routes['PUT /api/senders/domain/jobalerts.in'] = [200, { sender: { ...jobs, verdict: 'keep', rule_id: null, source: 'user' } }];
  await m.setRouting(row(jobs), 'keep');
  expect(m.senders.learned).toEqual([]);
});

it('loadFolders merges the mailboxes by name, offers trash, sent and drafts, leaves out Inbox and a list that fails, and drops a stale answer', async () => {
  const f = (...names: string[]): Reply => [200, { items: names.map((n) => ({ name: n.split(':')[0], delimiter: '/', special_use: n.split(':')[1] ?? '' })) }];
  routes['GET /api/accounts/1/folders'] = f('INBOX', 'Receipts', 'Archive:\\Archive', 'Junk:\\Junk', 'Deleted Messages:\\Trash', 'Sent Messages:\\Sent');
  routes['GET /api/accounts/2/folders'] = f('Inbox', 'Receipts', 'Clients', 'Drafts:\\Drafts', 'Sent Messages');
  routes['GET /api/accounts/3/folders'] = failed(502, 'mail_error', 'The mail server did not answer.');
  await m.loadFolders([1, 2, 3]);
  expect(m.senders.folders).toEqual(['Archive', 'Clients', 'Deleted Messages', 'Drafts', 'Junk', 'Receipts', 'Sent Messages']);

  let answerFirst = (_r: Response) => {};
  fetchMock.mockImplementationOnce(() => new Promise((resolve) => (answerFirst = resolve)));
  const first = m.loadFolders([1]);
  await m.loadFolders([2]);
  answerFirst(new Response(JSON.stringify({ items: [{ name: 'Stale', delimiter: '/', special_use: '' }] })));
  await first;
  expect(m.senders.folders).toEqual(['Clients', 'Drafts', 'Receipts', 'Sent Messages']);
});
