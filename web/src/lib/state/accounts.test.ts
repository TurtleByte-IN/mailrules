import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Account, TestResult } from '../api/accounts';
import { dispatch } from '../api/events';
import { accounts, connect, load, reconnect, remove, setPaused, statuses, testSummary } from './accounts.svelte';
import { toast } from './toast.svelte';

type Reply = [status: number, body?: unknown];

/** Stubs fetch with one reply per "METHOD /api/path"; any other request fails the test. */
function serve(routes: Record<string, Reply>) {
  const f = vi.fn(async (url: string, init: RequestInit) => {
    const r = routes[`${init.method} ${url}`];
    if (!r) throw new Error(`unexpected ${init.method} ${url}`);
    return new Response(r[1] === undefined ? null : JSON.stringify(r[1]), { status: r[0] });
  });
  vi.stubGlobal('fetch', f);
  return f;
}

const acct = (over: Partial<Account> = {}): Account => ({
  id: 1,
  label: 'me@icloud.com',
  preset: 'icloud',
  host: 'imap.mail.me.com',
  port: 993,
  tls_mode: 'implicit',
  username: 'me',
  watch_folder: 'INBOX',
  status: 'live',
  last_error: '',
  last_event_at: 1791270000, last_mail_at: null,
  capabilities: ['IMAP4rev1', 'IDLE', 'MOVE'],
  can_move: true,
  folder_count: 14,
  created_at: 1791260000,
  ...over,
});

beforeEach(() => Object.assign(accounts, { list: [acct(), acct({ id: 2, label: 'work@fastmail.com', preset: 'fastmail' })], loaded: true, error: '' }));
afterEach(() => vi.unstubAllGlobals());

describe('load', () => {
  it('takes the list from the daemon', async () => {
    serve({ 'GET /api/accounts': [200, { items: [acct({ id: 7 })] }] });
    await load();
    expect(accounts).toMatchObject({ list: [{ id: 7 }], loaded: true, error: '' });
  });

  it('keeps a real empty list apart from a failed load, and recovers on retry', async () => {
    Object.assign(accounts, { list: [], loaded: false });
    serve({ 'GET /api/accounts': [500, { error: { code: 'internal', message: 'Something went wrong.' } }] });
    await load();
    expect(accounts).toMatchObject({ list: [], loaded: false, error: 'Something went wrong.' });

    serve({ 'GET /api/accounts': [200, { items: [] }] });
    await load();
    expect(accounts).toMatchObject({ list: [], loaded: true, error: '' });
  });

  it('reports a daemon that cannot be reached', async () => {
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')));
    await load();
    expect(accounts.error).toBe('Failed to fetch');
  });
});

it('creates a mailbox, sending the password once and keeping none of it', async () => {
  const f = serve({ 'POST /api/accounts': [201, { account: acct({ id: 3, label: 'new@icloud.com', status: 'new' }) }] });
  const a = await connect({ preset: 'icloud', username: 'new@icloud.com', password: 'abcd-efgh-ijkl-mnop' });

  expect(JSON.parse(f.mock.calls[0][1].body as string)).toEqual({ preset: 'icloud', username: 'new@icloud.com', password: 'abcd-efgh-ijkl-mnop' });
  expect(a.id).toBe(3);
  expect(accounts.list.map((x) => x.id)).toEqual([1, 2, 3]);
  expect(toast.text).toBe('new@icloud.com is live');
  expect(JSON.stringify([accounts, { ...localStorage }, { ...sessionStorage }])).not.toContain('abcd-efgh-ijkl-mnop');
});

it.each<[string, () => Promise<void>, string, unknown, Reply, string, (Account['status'] | undefined)[]]>([
  ['reconnect', () => reconnect(1), 'POST /api/accounts/1/reconnect', undefined, [200, { account: acct({ status: 'reconnecting' }) }], 'Reconnecting me@icloud.com', ['reconnecting', 'live']],
  ['pause', () => setPaused(1, true), 'PATCH /api/accounts/1', { paused: true }, [200, { account: acct({ status: 'paused' }) }], 'me@icloud.com paused', ['paused', 'live']],
  ['resume', () => setPaused(1, false), 'PATCH /api/accounts/1', { paused: false }, [200, { account: acct({ status: 'new' }) }], 'me@icloud.com resumed', ['new', 'live']],
  ['remove', () => remove(1), 'DELETE /api/accounts/1', undefined, [204], 'me@icloud.com removed', ['live']],
])('%s calls the daemon and shows the result', async (_name, run, route, body, reply, said, statusesAfter) => {
  const f = serve({ [route]: reply });
  await run();
  const sent = f.mock.calls[0][1].body;
  expect(sent === undefined ? undefined : JSON.parse(sent as string)).toEqual(body);
  expect(toast.text).toBe(said);
  expect(accounts.list.map((a) => a.status)).toEqual(statusesAfter);
});

it.each<[string, () => Promise<void>, string, Reply, string]>([
  ['reconnect of a paused mailbox', () => reconnect(1), 'POST /api/accounts/1/reconnect', [409, { error: { code: 'account_paused', message: 'This account is paused. Resume it first.' } }], 'This account is paused. Resume it first.'],
  ['remove of a mailbox that is gone', () => remove(1), 'DELETE /api/accounts/1', [404, { error: { code: 'not_found', message: 'No such account.' } }], 'No such account.'],
  ['pause with no answer', () => setPaused(1, true), 'PATCH /api/accounts/1', [502], 'The daemon did not answer.'],
])('a failed %s shows the daemon message and changes nothing', async (_name, run, route, reply, said) => {
  serve({ [route]: reply });
  await run();
  expect(toast.text).toBe(said);
  expect(accounts.list).toEqual([acct(), acct({ id: 2, label: 'work@fastmail.com', preset: 'fastmail' })]);
});

it('follows account.status events for listed mailboxes only', () => {
  dispatch('account.status', acct({ id: 2, status: 'auth_failed', last_error: 'The mail server refused the sign-in.' }));
  dispatch('account.status', acct({ id: 9 }));
  expect(accounts.list.map((a) => [a.id, a.status])).toEqual([[1, 'live'], [2, 'auth_failed']]);
});

it('has a label and a dot for every status in the contract', () => {
  expect(Object.keys(statuses).sort()).toEqual(['auth_failed', 'error', 'live', 'new', 'paused', 'reconnecting']);
});

const folders = (...special: TestResult['folders'][number]['special_use'][]) =>
  [{ name: 'INBOX', delimiter: '/', special_use: '' as const }, ...special.map((s) => ({ name: s.slice(1), delimiter: '/', special_use: s }))];

it.each<[string, Omit<TestResult, 'username'>, string]>([
  ['everything', { folders: folders('\\Junk', '\\Trash', '\\Archive'), can_move: true, idle: true }, 'Connected. 4 folders found; Junk, Trash and Archive detected. Push (IDLE) supported.'],
  ['no special folders, no push', { folders: folders(), can_move: true, idle: false }, 'Connected. 1 folders found. No push here: new mail is checked for once a minute.'],
  ['cannot move', { folders: folders('\\Trash'), can_move: false, idle: true }, 'Connected. 2 folders found; Trash detected. Push (IDLE) supported. This server cannot move mail: rules can flag and mark it only.'],
])('testSummary: %s', (_name, r, said) => expect(testSummary({ username: 'me', ...r })).toBe(said));
