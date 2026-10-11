import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Account } from '../lib/api/accounts';
import { accounts, load } from '../lib/state/accounts.svelte';
import { auth } from '../lib/state/auth.svelte';
import { toast } from '../lib/state/toast.svelte';
import Accounts from './Accounts.svelte';

const SECRET = 'abcd-efgh-ijkl-mnop';

// GET /api/accounts, one item.
const acct = (over: Partial<Account> = {}): Account => ({
  id: 1, label: 'me@icloud.com', preset: 'icloud', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', username: 'me', watch_folder: 'INBOX',
  status: 'live', last_error: '', last_event_at: 1791270000, last_mail_at: null, capabilities: ['IMAP4rev1', 'IDLE', 'MOVE'], can_move: true, folder_count: 3, created_at: 1791260000, shared: false, mine: true, cert_fingerprint: '', one_click: false,
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
  await waitFor(() => expect(within(form.getByLabelText('Watched folder')).getAllByRole('option')).toHaveLength(3));
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
  auth.members = 1;
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

it.each([
  ['a free build', null, null, 'Any IMAP mailbox that signs in with a password or app password. MailRules sorts on the server, so every app you use sees the result.'],
  ['a build that offers one-click sign-in', '/api/oauth/start?provider=gmail', null, 'Any IMAP mailbox. MailRules sorts on the server, so every app you use sees the result.'],
  ["a build with a module's sign-in form", null, '/api/proton/sign-in', 'Any IMAP mailbox. MailRules sorts on the server, so every app you use sees the result.'],
])('says which mailboxes it takes in %s', async (_, url, form, said) => {
  accounts.presets = [{ name: 'gmail', label: 'Gmail', host: 'imap.gmail.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false, secret_label: 'App password', password: true, one_click_url: url, form_url: form }];
  await show();
  expect(screen.getByRole('heading', { name: 'Mailboxes' }).nextElementSibling!.textContent!.trim()).toBe(said);
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

it('says, before removing a mailbox, that its rules are kept but switched off, and removes nothing until confirmed', async () => {
  await show(acct());
  await fireEvent.click(screen.getByRole('button', { name: 'Remove me@icloud.com' }));
  const text = screen.getByRole('alert').textContent ?? '';
  expect(text).toContain('Rules that apply only to this mailbox are kept but switched off, marked so you can give them another mailbox.');
  expect(text).not.toContain('deletes its password, folder list, contacts, activity, undo history and the rules');
  expect(fetchMock.mock.calls.filter((c) => c[1].method === 'DELETE')).toHaveLength(0);
});

it('does not offer sharing in a team of one', async () => {
  await show(acct());
  const form = await openEdit();
  expect(form.queryByRole('button', { name: 'Shared with team' })).toBeNull();
});

it.each([
  acct({ mine: false, shared: true }),
  acct({ mine: false, shared: true, status: 'paused' }),
  acct({ mine: false, shared: true, status: 'auth_failed' }),
  acct({ mine: false, shared: true, status: 'cert_changed' }),
])(
  'offers no way to manage a mailbox a teammate added and shared ($status)',
  async (a) => {
    auth.members = 2;
    await show(a);
    expect(screen.getByText('me@icloud.com')).toBeTruthy();
    expect(screen.queryAllByRole('button', { name: /me@icloud\.com/ })).toEqual([]);
  },
);

it('shares the mailbox with the team from Edit when the team has more than one person', async () => {
  auth.members = 2;
  await show(acct());
  routes['PATCH /api/accounts/1'] = [200, { account: acct({ shared: true }) }];
  const form = await openEdit();
  const sharing = form.getByRole('button', { name: 'Shared with team' });
  expect(sharing.getAttribute('aria-pressed')).toBe('false');
  await fireEvent.click(sharing);
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(screen.queryByRole('form')).toBeNull());
  expect(patches()).toEqual([{ shared: true }]);
  expect(accounts.list[0].shared).toBe(true);
});

const FP = 'AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89:AB:CD:EF:01:23:45:67:89';
const cert = { fingerprint: FP, subject: 'CN=127.0.0.1', issuer: 'CN=127.0.0.1', not_before: 1700000000, not_after: 2000000000 };
const changed = acct({ status: 'cert_changed', last_error: 'the certificate of 127.0.0.1 changed', cert_fingerprint: '00:11' });

it('shows a changed certificate for checking, and accepting it reconnects the mailbox with it', async () => {
  await show(changed);
  expect(screen.getByText("The server's certificate changed. Check the new one before MailRules connects again.")).toBeTruthy();
  expect(screen.getByText(/watching INBOX/).textContent).not.toContain('the certificate of');
  routes['POST /api/accounts/1/test'] = [422, { error: { code: 'cert_changed', message: 'The certificate of 127.0.0.1 is not the one accepted for this mailbox.', path: 'cert_fingerprint', cert } }];
  routes['PATCH /api/accounts/1'] = [200, { account: acct({ status: 'new', cert_fingerprint: FP }) }];
  await fireEvent.click(screen.getByRole('button', { name: 'Check certificate for me@icloud.com' }));

  const check = within(await screen.findByRole('alert', { name: 'Server certificate' }));
  expect(check.getByText('The certificate of 127.0.0.1 is not the one accepted for this mailbox.')).toBeTruthy();
  expect(check.getByText(FP)).toBeTruthy();
  await fireEvent.click(check.getByRole('button', { name: 'Accept certificate' }));
  await waitFor(() => expect(toast.text).toBe('Certificate accepted. Reconnecting me@icloud.com'));
  expect(patches()).toEqual([{ cert_fingerprint: FP }]);
  expect(accounts.list[0]).toMatchObject({ status: 'new', cert_fingerprint: FP });
  expect(screen.queryByRole('alert', { name: 'Server certificate' })).toBeNull();
});

it('reconnects at once when the check finds the server presents the accepted certificate again', async () => {
  await show(changed);
  routes['POST /api/accounts/1/test'] = [200, found];
  routes['POST /api/accounts/1/reconnect'] = [200, { account: acct({ status: 'new' }) }];
  await fireEvent.click(screen.getByRole('button', { name: 'Check certificate for me@icloud.com' }));
  await waitFor(() => expect(toast.text).toBe('Reconnecting me@icloud.com'));
  expect(patches()).toEqual([]);
});

it('adds a Proton Mail mailbox through Bridge: the server is filled in, and its certificate is accepted before it connects', async () => {
  routes['GET /api/presets'] = [200, { items: [
    { name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: true, secret_label: 'App-specific password', password: true, one_click_url: null },
    { name: 'proton', label: 'Proton Mail', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', help_url: 'https://proton.me/support/protonmail-bridge-install', local_part_login: false, secret_label: 'Bridge password', password: true, one_click_url: null },
  ] }];
  accounts.presets = [];
  await show();
  await fireEvent.click(screen.getByRole('button', { name: 'Add mailbox' }));
  await fireEvent.click(await screen.findByRole('button', { name: /Proton Mail/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));

  expect((screen.getByLabelText('Host') as HTMLInputElement).value).toBe('127.0.0.1');
  expect((screen.getByLabelText('Port') as HTMLInputElement).value).toBe('1143');
  expect((screen.getByLabelText('Encryption') as HTMLSelectElement).value).toBe('starttls');
  expect(screen.getByText(/needs a paid Proton plan/).textContent).toContain('on this machine');
  expect(screen.getByText(/MailRules in Docker cannot reach a Bridge/)).toBeTruthy();
  expect(screen.getByRole('link', { name: 'How to set up Proton Mail Bridge' }).getAttribute('href')).toBe('https://proton.me/support/protonmail-bridge-install');

  routes['POST /api/accounts/test'] = [422, { error: { code: 'cert_untrusted', message: 'The certificate of 127.0.0.1 is not one this system trusts.', path: 'cert_fingerprint', cert } }];
  await fireEvent.input(screen.getByLabelText('Proton Mail email address'), { target: { value: 'me@proton.me' } });
  await fireEvent.input(screen.getByLabelText('Bridge password'), { target: { value: 'bridge-pw' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Test connection' }));
  const check = within(await screen.findByRole('alert', { name: 'Server certificate' }));
  expect(check.getByText(FP)).toBeTruthy();

  routes['POST /api/accounts/test'] = [200, found];
  await fireEvent.click(check.getByRole('button', { name: 'Accept certificate' }));
  expect(await screen.findByText(/^Connected\. 2 folders found/)).toBeTruthy();
  const tests = fetchMock.mock.calls.filter((c) => c[0] === '/api/accounts/test').map((c) => JSON.parse(c[1].body as string));
  expect(tests.map((b) => b.cert_fingerprint)).toEqual([undefined, FP]);
  expect(tests[1]).toMatchObject({ preset: 'proton', host: '127.0.0.1', port: 1143, tls_mode: 'starttls' });
});

it("adds a Proton Mail mailbox through the module's sign-in form: password, code, an error retried, then Rules", async () => {
  routes['GET /api/presets'] = [200, { items: [
    { name: 'proton', label: 'Proton Mail', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', help_url: '', local_part_login: false, secret_label: 'Bridge password', password: false, one_click_url: null, form_url: '/api/proton/sign-in' },
  ] }];
  routes['GET /api/accounts'] = [200, { items: [acct({ id: 5, label: 'me@proton.me', preset: 'proton' })] }];
  accounts.presets = [];
  await show();
  await fireEvent.click(screen.getByRole('button', { name: 'Add mailbox' }));
  // One Proton tile, the form's: no Bridge-password tile beside it.
  const tile = await screen.findByRole('button', { name: /Proton Mail/ });
  expect(tile.textContent).toContain('Address and password');
  expect(screen.queryByRole('button', { name: /Bridge password/ })).toBeNull();
  await fireEvent.click(tile);
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(screen.getByText(/Bridge needs a paid Proton plan/)).toBeTruthy();
  expect(screen.queryByLabelText('Host')).toBeNull();

  const post = (r: unknown) => (routes['POST /api/proton/sign-in'] = [200, r]);
  post({ step: 'code', state: 's' });
  await fireEvent.input(screen.getByLabelText('Proton Mail address'), { target: { value: 'me@proton.me' } });
  await fireEvent.input(screen.getByLabelText('Proton Mail password'), { target: { value: 'pw' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Sign in' }));

  post({ step: 'code', state: 's', message: 'That code is wrong.' });
  await fireEvent.input(await screen.findByLabelText(/^Two-factor code/), { target: { value: '1' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect((await screen.findByText('That code is wrong.')).getAttribute('role')).toBe('alert');
  expect((screen.getByLabelText(/^Two-factor code/) as HTMLInputElement).value).toBe('');

  post({ step: 'mailbox_password', state: 's' });
  await fireEvent.input(screen.getByLabelText(/^Two-factor code/), { target: { value: '2' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  post({ account_id: 5 });
  await fireEvent.input(await screen.findByLabelText(/^Mailbox password/), { target: { value: 'mbox' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(await screen.findByRole('heading', { name: 'Start with a few rules' })).toBeTruthy();
  expect(fetchMock.mock.calls.filter((c) => c[0] === '/api/proton/sign-in').map((c) => JSON.parse(c[1].body as string))).toEqual([
    { step: 'sign_in', username: 'me@proton.me', password: 'pw' },
    { step: 'code', state: 's', code: '1' },
    { step: 'code', state: 's', code: '2' },
    { step: 'mailbox_password', state: 's', password: 'mbox' },
  ]);
});

const bridge = acct({ preset: 'proton', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', cert_fingerprint: '00:11' });
const other = acct({ preset: 'generic', host: 'mail.example.com', port: 143, tls_mode: 'starttls', cert_fingerprint: '00:11' });

it('offers no server fields in Edit for a provider with its own server', async () => {
  await show(acct());
  const form = await openEdit();
  expect(form.queryByLabelText('Host')).toBeNull();
  expect(form.queryByLabelText('Port')).toBeNull();
  expect(form.queryByLabelText('Encryption')).toBeNull();
});

it("moves a Bridge mailbox to another port: the new server's certificate is shown and accepted before anything is saved", async () => {
  await show(bridge);
  const form = await openEdit();
  expect([(form.getByLabelText('Host') as HTMLInputElement).value, (form.getByLabelText('Port') as HTMLInputElement).value]).toEqual(['127.0.0.1', '1143']);
  await fireEvent.input(form.getByLabelText('Port'), { target: { value: '1144' } });
  await fireEvent.input(form.getByLabelText(/New app password/), { target: { value: SECRET } });
  routes['PATCH /api/accounts/1'] = [422, { error: { code: 'cert_untrusted', message: 'The certificate of 127.0.0.1 is not one this system trusts.', path: 'cert_fingerprint', cert } }];
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));

  const check = within(await form.findByRole('alert', { name: 'Server certificate' }));
  expect(check.getByText(FP)).toBeTruthy();
  expect(accounts.list[0]).toMatchObject({ port: 1143, cert_fingerprint: '00:11' });
  routes['PATCH /api/accounts/1'] = [200, { account: { ...bridge, port: 1144, cert_fingerprint: FP, status: 'new' } }];
  await fireEvent.click(check.getByRole('button', { name: 'Accept certificate' }));
  await waitFor(() => expect(screen.queryByRole('form')).toBeNull());
  expect(patches()).toEqual([{ port: 1144, password: SECRET }, { port: 1144, password: SECRET, cert_fingerprint: FP }]);
  expect(accounts.list[0]).toMatchObject({ port: 1144, cert_fingerprint: FP });
});

it('keeps the form open and the mailbox as it was when the new server cannot be reached', async () => {
  await show(bridge);
  const form = await openEdit();
  await fireEvent.input(form.getByLabelText('Host'), { target: { value: ' 127.0.0.2 ' } });
  routes['PATCH /api/accounts/1'] = [422, { error: { code: 'connection_failed', message: 'Could not reach Proton Mail Bridge at 127.0.0.2:1143.', path: 'host' } }];
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  expect((await form.findByText('Could not reach Proton Mail Bridge at 127.0.0.2:1143.')).getAttribute('role')).toBe('alert');
  expect(form.getByLabelText(/^Host/).getAttribute('aria-invalid')).toBe('true');
  expect(patches()).toEqual([{ host: '127.0.0.2' }]);
  expect(accounts.list[0].host).toBe('127.0.0.1');
});

it("changes a Zoho mailbox's region in Edit", async () => {
  await show(acct({ preset: 'zoho', host: 'imap.zoho.com' }));
  routes['PATCH /api/accounts/1'] = [200, { account: acct({ preset: 'zoho', host: 'imap.zoho.eu' }) }];
  const form = await openEdit();
  expect(form.queryByLabelText('Port')).toBeNull();
  expect(form.queryByLabelText('Encryption')).toBeNull();
  await fireEvent.change(form.getByLabelText('Zoho region'), { target: { value: 'imap.zoho.eu' } });
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  await waitFor(() => expect(screen.queryByRole('form')).toBeNull());
  expect(patches()).toEqual([{ host: 'imap.zoho.eu' }]);
});

it('moves the port with the encryption for a server the person named, until the port is typed by hand', async () => {
  await show(other);
  const form = await openEdit();
  const port = form.getByLabelText('Port') as HTMLInputElement;
  const tls = form.getByLabelText('Encryption') as HTMLSelectElement;
  expect([tls.value, port.value]).toEqual(['starttls', '143']);
  await fireEvent.change(tls, { target: { value: 'implicit' } });
  expect(port.value).toBe('993');
  await fireEvent.input(port, { target: { value: '1993' } });
  await fireEvent.change(tls, { target: { value: 'starttls' } });
  await fireEvent.change(tls, { target: { value: 'implicit' } });
  expect(port.value).toBe('1993');
});

it("switches a Bridge mailbox to SSL: the port stays, and the new server's certificate is accepted before anything is saved", async () => {
  await show(bridge);
  const form = await openEdit();
  const tls = form.getByLabelText('Encryption') as HTMLSelectElement;
  expect([...tls.options].map((o) => o.text)).toEqual(['SSL', 'STARTTLS']);
  await fireEvent.change(tls, { target: { value: 'implicit' } });
  expect((form.getByLabelText('Port') as HTMLInputElement).value).toBe('1143');
  routes['PATCH /api/accounts/1'] = [422, { error: { code: 'cert_untrusted', message: 'The certificate of 127.0.0.1 is not one this system trusts.', path: 'cert_fingerprint', cert } }];
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));

  const check = within(await form.findByRole('alert', { name: 'Server certificate' }));
  expect(accounts.list[0]).toMatchObject({ tls_mode: 'starttls', cert_fingerprint: '00:11' });
  routes['PATCH /api/accounts/1'] = [200, { account: { ...bridge, tls_mode: 'implicit', cert_fingerprint: FP, status: 'new' } }];
  await fireEvent.click(check.getByRole('button', { name: 'Accept certificate' }));
  await waitFor(() => expect(screen.queryByRole('form')).toBeNull());
  expect(patches()).toEqual([{ tls_mode: 'implicit' }, { tls_mode: 'implicit', cert_fingerprint: FP }]);
  expect(accounts.list[0]).toMatchObject({ tls_mode: 'implicit', cert_fingerprint: FP });
});

it('shows a refused encryption beside the Encryption field and keeps the mailbox as it was', async () => {
  await show(other);
  const form = await openEdit();
  await fireEvent.change(form.getByLabelText('Encryption'), { target: { value: 'implicit' } });
  routes['PATCH /api/accounts/1'] = [400, { error: { code: 'invalid_input', message: 'The TLS mode is implicit or starttls.', path: 'tls_mode' } }];
  await fireEvent.click(form.getByRole('button', { name: 'Save' }));
  expect((await form.findByText('The TLS mode is implicit or starttls.')).getAttribute('role')).toBe('alert');
  expect(form.getByLabelText(/^Encryption/).getAttribute('aria-invalid')).toBe('true');
  expect(patches()).toEqual([{ port: 993, tls_mode: 'implicit' }]);
  expect(accounts.list[0].tls_mode).toBe('starttls');
});

it("sends a one-click mailbox that needs reconnecting to its provider's sign-in, and offers it no password", async () => {
  routes['GET /api/presets'] = [200, { items: [{ name: 'gmail', label: 'Gmail', host: 'imap.gmail.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false,
    secret_label: 'App password', password: true, one_click_url: '/api/oauth/start?provider=gmail' }] }];
  accounts.presets = [];
  const assign = vi.fn();
  vi.stubGlobal('location', { ...window.location, assign });
  await show(acct({ label: 'me@icloud.com', preset: 'gmail', one_click: true, status: 'reconnect_needed', last_error: 'invalid_grant' }));
  await waitFor(() => expect(screen.getByText(/Gmail no longer accepts MailRules' sign-in/)).toBeTruthy());
  expect(screen.getByText('Reconnect needed')).toBeTruthy();
  expect(screen.queryByText(/invalid_grant/)).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Reconnect me@icloud.com' }));
  expect(assign).toHaveBeenCalledWith('/api/oauth/start?provider=gmail&account=1');
  expect(fetchMock.mock.calls.some((c) => c[0] === '/api/accounts/1/reconnect')).toBe(false);
  const form = await openEdit();
  expect(form.queryByLabelText('New app password')).toBeNull();
});

it('tells a teammate that the person who added a one-click mailbox needs to reconnect it', async () => {
  await show(acct({ mine: false, shared: true, one_click: true, status: 'reconnect_needed' }));
  expect(screen.getByText(/The person who added this mailbox needs to reconnect it/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Reconnect/ })).toBeNull();
});
