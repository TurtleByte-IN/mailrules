import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Account } from '../lib/api/accounts';
import { accounts, load } from '../lib/state/accounts.svelte';
import { toast } from '../lib/state/toast.svelte';
import Accounts from './Accounts.svelte';

const SECRET = 'abcd-efgh-ijkl-mnop';

// GET /api/accounts, one item.
const acct = (over: Partial<Account> = {}): Account => ({
  id: 1, label: 'me@icloud.com', preset: 'icloud', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', username: 'me', watch_folder: 'INBOX',
  status: 'live', last_error: '', last_event_at: 1791270000, last_mail_at: null, capabilities: ['IMAP4rev1', 'IDLE', 'MOVE'], can_move: true, folder_count: 3, created_at: 1791260000,
  ...over,
});
const failed = acct({ status: 'auth_failed', last_error: 'The mail server refused the sign-in.' });

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const r = routes[`${init.method} ${url}`];
  if (!r) throw new Error(`unexpected ${init.method} ${url}`);
  return new Response(JSON.stringify(r[1]), { status: r[0] });
});
const patches = () => fetchMock.mock.calls.filter((c) => c[1].method === 'PATCH').map((c) => JSON.parse(c[1].body as string));

/** Mounts the screen the way the shell does: the list is loaded first. */
async function show(...list: Account[]) {
  routes['GET /api/accounts'] = [200, { items: list }];
  await load();
  render(Accounts);
}

/** Opens Edit on the one mailbox and waits for its folders. */
async function openEdit(button = 'Edit me@icloud.com') {
  await fireEvent.click(screen.getByRole('button', { name: button }));
  const form = within(screen.getByRole('form', { name: 'Edit me@icloud.com' }));
  await waitFor(() => expect(form.getAllByRole('option')).toHaveLength(3));
  return form;
}

beforeEach(() => {
  routes = {
    'GET /api/presets': [200, { items: [{ name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: true }] }],
    'GET /api/accounts/1/folders': [200, { items: [{ name: 'Archive', delimiter: '/', special_use: '\\Archive' }, { name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Work', delimiter: '/', special_use: '' }] }],
  };
  fetchMock.mockClear();
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(accounts, { list: [], loaded: false, error: '' });
  toast.text = '';
});
afterEach(() => vi.unstubAllGlobals());

it('shows a failed load with Retry, and Retry fetches the list again', async () => {
  routes['GET /api/accounts'] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  await load();
  render(Accounts);
  expect(screen.getByRole('alert').textContent).toContain('Could not load your mailboxes. Something went wrong.');
  expect(screen.queryByText('No mailbox connected yet.')).toBeNull();

  routes['GET /api/accounts'] = [200, { items: [] }];
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByText('No mailbox connected yet.');
  expect(screen.queryByRole('alert')).toBeNull();
  expect(fetchMock.mock.calls.filter((c) => c[0] === '/api/accounts')).toHaveLength(2);
});

it('says so when no mailbox is connected', async () => {
  await show();
  expect(screen.getByText('No mailbox connected yet.')).toBeTruthy();
});

it('asks for a new app password on a mailbox whose sign-in failed, and opens Edit on that field', async () => {
  await show(failed);
  expect(screen.getByText('Sign-in failed. Enter a new app password.')).toBeTruthy();
  const form = await openEdit('New app password for me@icloud.com');
  const field = form.getByLabelText(/New app password/) as HTMLInputElement;
  expect(document.activeElement).toBe(field);
  expect([field.type, field.autocomplete]).toEqual(['password', 'new-password']);
});

it('has no new-password prompt on a mailbox that is signed in', async () => {
  await show(acct());
  expect(screen.queryByText('Sign-in failed. Enter a new app password.')).toBeNull();
});

it('fills Edit from the mailbox and its folders, and sends only the fields that changed', async () => {
  await show(acct());
  routes['PATCH /api/accounts/1'] = [200, { account: acct({ label: 'Home', watch_folder: 'Work' }) }];
  const form = await openEdit();
  const name = form.getByLabelText('Name') as HTMLInputElement;
  const folder = form.getByLabelText('Watched folder') as HTMLSelectElement;
  expect([name.value, folder.value, document.activeElement]).toEqual(['me@icloud.com', 'INBOX', name]);

  await fireEvent.input(name, { target: { value: ' Home ' } });
  await fireEvent.change(folder, { target: { value: 'Work' } });
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));

  await waitFor(() => expect(toast.text).toBe('Home updated'));
  expect(patches()).toEqual([{ label: 'Home', watch_folder: 'Work' }]);
  expect(screen.queryByRole('form')).toBeNull();
  expect(screen.getByText('Home')).toBeTruthy();
  expect(screen.getByText(/watching Work/)).toBeTruthy();
});

it('sends a new password once, in the request body, and keeps it nowhere', async () => {
  await show(failed);
  routes['PATCH /api/accounts/1'] = [200, { account: acct({ status: 'new' }) }];
  const form = await openEdit();
  await fireEvent.input(form.getByLabelText(/New app password/), { target: { value: SECRET } });
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));

  await waitFor(() => expect(toast.text).toBe('me@icloud.com updated'));
  expect(patches()).toEqual([{ password: SECRET }]);
  expect(fetchMock.mock.calls.filter((c) => JSON.stringify(c).includes(SECRET)).map((c) => [c[1].method, c[0]])).toEqual([['PATCH', '/api/accounts/1']]);
  expect(JSON.stringify([accounts, toast, { ...localStorage }, { ...sessionStorage }, document.cookie, location.href])).not.toContain(SECRET);
  expect(document.body.innerHTML).not.toContain(SECRET);
  expect(screen.queryByText('Sign-in failed. Enter a new app password.')).toBeNull();
});

it('shows a refusal beside the field it names, clears the password and keeps the form open', async () => {
  await show(failed);
  routes['PATCH /api/accounts/1'] = [400, { error: { code: 'invalid_input', message: 'The mail server refused the sign-in.', path: 'password' } }];
  const form = await openEdit();
  const field = form.getByLabelText(/New app password/) as HTMLInputElement;
  await fireEvent.input(field, { target: { value: SECRET } });
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));

  expect((await form.findByRole('alert')).textContent).toBe('The mail server refused the sign-in.');
  expect([field.value, field.getAttribute('aria-invalid'), toast.text]).toEqual(['', 'true', '']);
  expect(accounts.list).toEqual([failed]);
});

it('shows a refusal that names no field as a toast', async () => {
  await show(acct());
  routes['PATCH /api/accounts/1'] = [404, { error: { code: 'not_found', message: 'No such account.' } }];
  const form = await openEdit();
  await fireEvent.input(form.getByLabelText('Name'), { target: { value: 'Home' } });
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(toast.text).toBe('No such account.'));
  expect(form.queryByRole('alert')).toBeNull();
});

it('asks the daemon for nothing when Save is pressed with no change, or on Cancel', async () => {
  await show(acct());
  let form = await openEdit();
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  expect(screen.queryByRole('form')).toBeNull();

  form = await openEdit();
  await fireEvent.input(form.getByLabelText(/New app password/), { target: { value: SECRET } });
  await fireEvent.click(form.getByRole('button', { name: 'Cancel' }));
  expect(screen.queryByRole('form')).toBeNull();
  form = await openEdit();
  expect((form.getByLabelText(/New app password/) as HTMLInputElement).value).toBe('');
  expect(patches()).toEqual([]);
  expect(document.body.innerHTML).not.toContain(SECRET);
});

it('still lets the password be changed when the folders cannot be listed', async () => {
  await show(failed);
  routes['GET /api/accounts/1/folders'] = [404, { error: { code: 'not_found', message: 'No such account.' } }];
  await fireEvent.click(screen.getByRole('button', { name: 'Edit me@icloud.com' }));
  await waitFor(() => expect(toast.text).toBe('No such account.'));
  expect(screen.getAllByRole('option').map((o) => o.textContent)).toEqual(['INBOX']);
});

// POST /api/accounts/{id}/test, 200.
const found = { username: 'me', folders: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Trash', delimiter: '/', special_use: '\\Trash' }], can_move: true, idle: true };

it('offers Test in place of Reconnect on a live mailbox, and Reconnect on any other', async () => {
  await show(acct(), acct({ id: 2, label: 'work@fastmail.com', status: 'error', last_error: 'The mail server closed the connection.' }));
  expect(screen.getByRole('button', { name: 'Test me@icloud.com' }).textContent).toBe('Test');
  expect(screen.queryByRole('button', { name: 'Reconnect me@icloud.com' })).toBeNull();
  expect(screen.getByRole('button', { name: 'Reconnect work@fastmail.com' })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Test work@fastmail.com' })).toBeNull();
});

it.each<[string, Reply, string]>([
  ['passes', [200, found], 'Connected. 2 folders found; Trash detected. Push (IDLE) supported.'],
  ['fails', [422, { error: { code: 'auth_failed', message: 'The mail server refused the sign-in.' } }], 'The mail server refused the sign-in.'],
])('Test reads "Testing…" while it runs, then shows how it went when it %s; the mailbox stays as it was', async (_name, reply, said) => {
  await show(acct());
  let answer = () => {};
  fetchMock.mockImplementationOnce(() => new Promise((done) => (answer = () => done(new Response(JSON.stringify(reply[1]), { status: reply[0] })))));
  const button = screen.getByRole('button', { name: 'Test me@icloud.com' }) as HTMLButtonElement;
  await fireEvent.click(button);
  expect([button.textContent, button.disabled]).toEqual(['Testing…', true]);
  const [url, init] = fetchMock.mock.calls.at(-1)!;
  expect([init.method, url]).toEqual(['POST', '/api/accounts/1/test']);

  answer();
  await waitFor(() => expect(toast.text).toBe(said));
  expect([button.textContent, button.disabled]).toEqual(['Test', false]);
  expect(accounts.list).toEqual([acct()]);
});

it('says when the last email arrived only once one has', async () => {
  await show(acct(), acct({ id: 2, label: 'work@fastmail.com', last_mail_at: 1791273600 }));
  const rows = screen.getAllByText(/watching INBOX/).map((r) => r.textContent!);
  expect(rows[0]).toMatch(/· since \S.*, \S+/);
  expect(rows[0]).not.toContain('last email');
  expect(rows[1]).toMatch(/· since \S.*, \S.*· last email \S.*, \S+/);
});
