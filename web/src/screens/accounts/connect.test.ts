import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Account, Preset } from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, loadPresets } from '../../lib/state/accounts.svelte';
import { addTemplatesByName } from '../../lib/state/compose.svelte';
import { toast } from '../../lib/state/toast.svelte';
import { Wizard } from './connect.svelte';

vi.mock('../../lib/state/compose.svelte', () => ({ addTemplatesByName: vi.fn() }));

const SECRET = 'abcd-efgh-ijkl-mnop';

// GET /api/presets, first and last entry.
const presets: Preset[] = [
  { name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: 'https://support.apple.com/en-us/102654', local_part_login: true },
  { name: 'generic', label: 'Other IMAP server', host: '', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false },
];
const found = { username: 'new', folders: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Junk', delimiter: '/', special_use: '\\Junk' }], can_move: true, idle: true };
const created: Account = {
  id: 3, label: 'new@icloud.com', preset: 'icloud', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', username: 'new', watch_folder: 'INBOX',
  status: 'new', last_error: '', last_event_at: null, capabilities: ['IDLE', 'MOVE'], can_move: true, folder_count: 2, created_at: 1791260000,
};

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const r = routes[`${init.method} ${url}`];
  if (!r) throw new Error(`unexpected ${init.method} ${url}`);
  return new Response(JSON.stringify(r[1]), { status: r[0] });
});
const bodies = (path: string) => fetchMock.mock.calls.filter((c) => c[0] === path).map((c) => JSON.parse(c[1].body as string));
const refusal = (code: string, message: string, path?: string) => ({ error: { code, message, path } });

beforeEach(async () => {
  routes = {
    'GET /api/presets': [200, { items: presets }],
    'POST /api/accounts/test': [200, found],
    'POST /api/accounts': [201, { account: created }],
  };
  fetchMock.mockClear();
  vi.stubGlobal('fetch', fetchMock);
  vi.mocked(addTemplatesByName).mockReset().mockResolvedValue(0);
  accounts.list = [];
  await loadPresets();
});
afterEach(() => vi.unstubAllGlobals());

function atSignIn(fields: Partial<Pick<Wizard, 'presetId' | 'email' | 'password' | 'host' | 'port'>> = {}) {
  const w = new Wizard();
  Object.assign(w, { step: 1, ...fields });
  return w;
}

/** A wizard on the last step with a passed test. */
async function atGoLive() {
  const w = atSignIn({ email: 'new@icloud.com', password: SECRET });
  await w.runTest();
  w.step = 3;
  return w;
}

it.each<[string, Parameters<typeof atSignIn>[0]]>([
  ['no email', { password: SECRET }],
  ['no password', { email: 'a@icloud.com' }],
  ['blank password', { email: 'a@icloud.com', password: '   ' }],
  ['other IMAP without a host', { presetId: 'generic', email: 'a@example.com', password: SECRET }],
])('stays on sign-in with %s, without asking the daemon', async (_name, fields) => {
  const w = atSignIn(fields);
  expect(await w.next()).toBe(false);
  expect([w.step, w.test, w.errorField]).toEqual([1, 'err', '']);
  expect(w.error).toBe('Enter your email and the app-specific password first.');
  expect(bodies('/api/accounts/test')).toEqual([]);
});

it('does not leave the provider step until the presets have loaded', async () => {
  accounts.presets = [];
  const w = new Wizard();
  await w.next();
  expect(w.step).toBe(0);
});

it('sends host and port only for a preset that has no host of its own', async () => {
  await atSignIn({ email: 'a@icloud.com', password: SECRET, host: 'ignored.example', port: 143 }).runTest();
  await atSignIn({ presetId: 'generic', email: ' a@example.com ', password: SECRET, host: ' mail.example.com ', port: 143 }).runTest();
  expect(bodies('/api/accounts/test')).toEqual([
    { preset: 'icloud', username: 'a@icloud.com', password: SECRET },
    { preset: 'generic', username: 'a@example.com', password: SECRET, host: 'mail.example.com', port: 143 },
  ]);
});

it.each<[string, Wizard['presetId'], Reply, string]>([
  ['wrong password', 'icloud', [422, refusal('auth_failed', 'The mail server refused the sign-in.', 'password')], 'password'],
  ['missing username', 'icloud', [400, refusal('invalid_input', 'Enter the email address or username you sign in with.', 'username')], 'username'],
  ['unreachable host typed by the user', 'generic', [422, refusal('connection_failed', 'Could not reach 127.0.0.1:1. Check the host and port.', 'host')], 'host'],
  ['bad port', 'generic', [400, refusal('invalid_input', 'The port must be between 1 and 65535.', 'port')], 'port'],
  ['unreachable host of a preset, which has no host field', 'icloud', [422, refusal('connection_failed', 'Could not reach imap.mail.me.com:993. Check the host and port.', 'host')], ''],
  ['watch folder missing, which has no field', 'icloud', [422, refusal('no_folder', 'The server has no folder named INBOX.', 'watch_folder')], ''],
  ['an error with no path', 'icloud', [403, refusal('csrf_failed', 'Reload the page and try again.')], ''],
])('shows the daemon message inline when the test fails: %s', async (_name, presetId, reply, field) => {
  routes['POST /api/accounts/test'] = reply;
  const w = atSignIn({ presetId, email: 'd@example.com', password: SECRET, host: '127.0.0.1' });
  expect(await w.next()).toBe(false);
  expect([w.step, w.test, w.error, w.errorField]).toEqual([1, 'err', (reply[1] as ReturnType<typeof refusal>).error.message, field]);
});

it('tests first, then walks to the end and saves the mailbox', async () => {
  const w = new Wizard();
  const labels: string[] = [];
  const press = async () => {
    labels.push(w.nextLabel);
    return w.next();
  };

  await press();
  w.email = ' new@icloud.com ';
  w.password = SECRET;
  await press(); // runs the test, stays
  expect([w.step, w.test, w.result]).toEqual([1, 'ok', found]);
  await press();
  w.templates[0].on = false;
  w.templates[1].on = false;
  await press();
  expect(await press()).toBe(true);

  expect(labels).toEqual(['Continue', 'Test and continue', 'Continue', 'Preview with 1 rule', 'Go live']);
  expect(bodies('/api/accounts')).toEqual([{ preset: 'icloud', username: 'new@icloud.com', password: SECRET }]);
  expect(accounts.list).toEqual([created]);
  expect(addTemplatesByName).toHaveBeenCalledExactlyOnceWith(['Login codes']);
  expect(toast.text).toBe('new@icloud.com is live');
});

it('does not ask for starter rules when none is chosen', async () => {
  const w = await atGoLive();
  for (const t of w.templates) t.on = false;
  expect(await w.next()).toBe(true);
  expect(addTemplatesByName).not.toHaveBeenCalled();
});

it.each<[string, Error, string]>([
  ['are not built yet (501)', new ApiError(501, 'not_implemented', 'This part of MailRules is not built yet.'), 'new@icloud.com is live. Starter rules could not be added yet.'],
  ['fail', new ApiError(500, 'internal', 'Something went wrong.'), 'Something went wrong.'],
])('leaves the mailbox connected when the starter rules %s', async (_name, error, said) => {
  vi.mocked(addTemplatesByName).mockRejectedValue(error);
  const w = await atGoLive();
  expect(await w.next()).toBe(true);
  expect(accounts.list).toEqual([created]);
  expect([toast.text, w.busy]).toEqual([said, false]);
});

it('goes back to the sign-in form when the daemon refuses to save', async () => {
  routes['POST /api/accounts'] = [409, refusal('account_exists', 'This mailbox is already connected.', 'username')];
  const w = await atGoLive();
  expect(await w.next()).toBe(false);
  expect([w.step, w.test, w.error, w.errorField, w.password, w.busy]).toEqual([1, 'err', 'This mailbox is already connected.', 'username', '', false]);
  expect(accounts.list).toEqual([]);
  expect(addTemplatesByName).not.toHaveBeenCalled();
});

it('sends the password in two request bodies and keeps it nowhere else', async () => {
  const w = await atGoLive();
  expect(await w.next()).toBe(true);

  expect(w.password).toBe('');
  expect(fetchMock.mock.calls.filter((c) => JSON.stringify(c).includes(SECRET)).map((c) => c[0])).toEqual(['/api/accounts/test', '/api/accounts']);
  expect(JSON.stringify([accounts, { ...localStorage }, { ...sessionStorage }, document.cookie])).not.toContain(SECRET);
});

it('voids a passed test when a sign-in field changes, and will not save without one', async () => {
  const w = await atGoLive();
  w.edited();
  expect(w.test).toBe('idle');
  expect(await w.next()).toBe(false);
  expect(bodies('/api/accounts')).toEqual([]);
});

it('ignores a test result that arrives after the fields changed', async () => {
  const w = atSignIn({ email: 'c@icloud.com', password: SECRET });
  const running = w.runTest();
  w.edited();
  await running;
  expect(w.test).toBe('idle');
});
