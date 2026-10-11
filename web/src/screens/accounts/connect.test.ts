import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Account, FormStep, Preset } from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, loadPresets } from '../../lib/state/accounts.svelte';
import { addTemplatesByName } from '../../lib/state/compose.svelte';
import { toast } from '../../lib/state/toast.svelte';
import { Wizard } from './connect.svelte';

vi.mock('../../lib/state/compose.svelte', () => ({ addTemplatesByName: vi.fn() }));

const SECRET = 'abcd-efgh-ijkl-mnop';

// GET /api/presets, first and last entry.
const presets: Preset[] = [
  { name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: 'https://support.apple.com/en-us/102654', local_part_login: true, secret_label: 'App-specific password', password: true, one_click_url: null, form_url: null },
  { name: 'zoho', label: 'Zoho Mail', host: 'imap.zoho.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false, secret_label: 'App password', password: true, one_click_url: null, form_url: null },
  { name: 'proton', label: 'Proton Mail', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', help_url: 'https://proton.me/support/protonmail-bridge-install', local_part_login: false, secret_label: 'Bridge password', password: true, one_click_url: null, form_url: null },
  { name: 'generic', label: 'Other IMAP server', host: '', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false, secret_label: 'Password', password: true, one_click_url: null, form_url: null },
];
const found = { username: 'new', folders: [{ name: 'INBOX', delimiter: '/', special_use: '' }, { name: 'Junk', delimiter: '/', special_use: '\\Junk' }], can_move: true, idle: true };
const created: Account = {
  id: 3, label: 'new@icloud.com', preset: 'icloud', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', username: 'new', watch_folder: 'INBOX',
  status: 'new', last_error: '', last_event_at: null, last_mail_at: null, capabilities: ['IDLE', 'MOVE'], can_move: true, folder_count: 2, created_at: 1791260000, shared: false, mine: true, cert_fingerprint: '', one_click: false,
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

it('sends host, port and encryption only for a preset that has no host of its own', async () => {
  await atSignIn({ email: 'a@icloud.com', password: SECRET, host: 'ignored.example', port: 143 }).runTest();
  await atSignIn({ presetId: 'generic', email: ' a@example.com ', password: SECRET, host: ' mail.example.com ', port: 143 }).runTest();
  expect(bodies('/api/accounts/test')).toEqual([
    { preset: 'icloud', username: 'a@icloud.com', password: SECRET },
    { preset: 'generic', username: 'a@example.com', password: SECRET, host: 'mail.example.com', port: 143, tls_mode: 'implicit' },
  ]);
});

it.each([
  ['com', 'imap.zoho.com'],
  ['eu', 'imap.zoho.eu'],
  ['in', 'imap.zoho.in'],
  ['com.au', 'imap.zoho.com.au'],
  ['jp', 'imap.zoho.jp'],
  ['com.cn', 'imap.zoho.com.cn'],
])('sends the host of the chosen Zoho region %s: %s', async (region, host) => {
  const w = atSignIn({ presetId: 'zoho', email: 'a@zoho.eu', password: SECRET });
  w.region = region;
  await w.runTest();
  w.step = 3;
  await w.next();
  const sent = { preset: 'zoho', username: 'a@zoho.eu', password: SECRET, host };
  expect(bodies('/api/accounts/test')).toEqual([sent]);
  expect(bodies('/api/accounts')).toEqual([sent]);
});

it('shows a Zoho host failure beside the region, not beside a host field that is not there', async () => {
  routes['POST /api/accounts/test'] = [422, refusal('connection_failed', 'Could not reach imap.zoho.in:993. Check the host and port.', 'host')];
  const w = atSignIn({ presetId: 'zoho', email: 'a@zoho.in', password: SECRET });
  w.region = 'in';
  await w.runTest();
  expect(w.errorField).toBe('host');
});

it.each<[string, Wizard['presetId'], Reply, string]>([
  ['wrong password', 'icloud', [422, refusal('auth_failed', 'The mail server refused the sign-in.', 'password')], 'password'],
  ['missing username', 'icloud', [400, refusal('invalid_input', 'Enter the email address or username you sign in with.', 'username')], 'username'],
  ['unreachable host typed by the user', 'generic', [422, refusal('connection_failed', 'Could not reach 127.0.0.1:1. Check the host and port.', 'host')], 'host'],
  ['bad encryption mode', 'generic', [400, refusal('invalid_input', 'tls_mode must be implicit or starttls.', 'tls_mode')], 'tls_mode'],
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

it('moves the port with the encryption mode until the port is typed by hand', () => {
  const w = atSignIn({ presetId: 'generic' });
  expect([w.tls, w.port]).toEqual(['implicit', 993]);
  w.setTls('starttls');
  expect(w.port).toBe(143);
  w.setTls('implicit');
  expect(w.port).toBe(993);
  w.port = 2143;
  w.portEdited();
  w.setTls('starttls');
  expect([w.tls, w.port]).toEqual(['starttls', 2143]);
});

it('sends the encryption mode in the test and in the save, and a change voids the test', async () => {
  const w = atSignIn({ presetId: 'generic', email: 'a@example.com', password: SECRET, host: 'mail.example.com' });
  await w.runTest();
  w.setTls('starttls');
  expect(w.test).toBe('idle');
  await w.runTest();
  w.step = 3;
  expect(await w.next()).toBe(true);
  const sent = { preset: 'generic', username: 'a@example.com', password: SECRET, host: 'mail.example.com' };
  expect(bodies('/api/accounts/test')).toEqual([{ ...sent, port: 993, tls_mode: 'implicit' }, { ...sent, port: 143, tls_mode: 'starttls' }]);
  expect(bodies('/api/accounts')).toEqual([{ ...sent, port: 143, tls_mode: 'starttls' }]);
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

  expect(labels).toEqual(['Continue', 'Test and continue', 'Continue', 'Preview with 1 rule', 'Connect']);
  expect(bodies('/api/accounts')).toEqual([{ preset: 'icloud', username: 'new@icloud.com', password: SECRET }]);
  expect(accounts.list).toEqual([created]);
  expect(addTemplatesByName).toHaveBeenCalledExactlyOnceWith(['Login codes']);
  expect(toast.text).toBe('new@icloud.com is connected');
});

it('says it is connecting while the mailbox is being saved', async () => {
  const w = await atGoLive();
  let answer!: () => void;
  fetchMock.mockImplementationOnce(() => new Promise((resolve) => (answer = () => resolve(new Response(JSON.stringify({ account: created }), { status: 201 })))));
  const saved = w.next();
  expect(w.busy).toBe(true);
  expect(w.nextLabel).toBe('Connecting…');
  answer();
  expect(await saved).toBe(true);
  expect(w.nextLabel).toBe('Connect');
});

it('does not ask for starter rules when none is chosen', async () => {
  const w = await atGoLive();
  for (const t of w.templates) t.on = false;
  expect(await w.next()).toBe(true);
  expect(addTemplatesByName).not.toHaveBeenCalled();
});

it.each<[number, string]>([
  [3, 'new@icloud.com is connected with 3 new rules'],
  [1, 'new@icloud.com is connected with 1 new rule'],
  [0, 'new@icloud.com is connected'],
])('says how many starter rules were added: %i', async (added, said) => {
  vi.mocked(addTemplatesByName).mockResolvedValue(added);
  const w = await atGoLive();
  expect(await w.next()).toBe(true);
  expect(toast.text).toBe(said);
});

it('leaves the mailbox connected when the starter rules fail', async () => {
  vi.mocked(addTemplatesByName).mockRejectedValue(new ApiError(500, 'internal', 'Something went wrong.'));
  const w = await atGoLive();
  expect(await w.next()).toBe(true);
  expect(accounts.list).toEqual([created]);
  expect([toast.text, w.busy]).toEqual(['Something went wrong.', false]);
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

it('fills in Proton Mail Bridge on this machine, and sends it as the server, which can be changed', async () => {
  const w = atSignIn({ email: 'me@proton.me', password: 'bridge-pw' });
  w.choose('proton');
  expect([w.serverFields, w.host, w.port, w.tls]).toEqual([true, '127.0.0.1', 1143, 'starttls']);
  w.setTls('implicit'); // Bridge's SSL mode keeps its port
  expect(w.port).toBe(1143);
  w.setTls('starttls');
  await w.runTest();
  w.port = 1144;
  w.portEdited();
  await w.runTest();
  const sent = { preset: 'proton', username: 'me@proton.me', password: 'bridge-pw', host: '127.0.0.1', tls_mode: 'starttls' };
  expect(bodies('/api/accounts/test')).toEqual([{ ...sent, port: 1143 }, { ...sent, port: 1144 }]);
  w.choose('generic');
  expect([w.host, w.port, w.tls]).toEqual(['', 993, 'implicit']);
});

const cert = { fingerprint: 'AB:CD', subject: 'CN=127.0.0.1', issuer: 'CN=127.0.0.1', not_before: 1700000000, not_after: 2000000000 };
const certRefusal = (code: string) => ({ error: { code, message: 'The certificate of 127.0.0.1 is not one this system trusts.', path: 'cert_fingerprint', cert } });

it('shows a certificate the system does not trust, and tests and saves with it once accepted', async () => {
  routes['POST /api/accounts/test'] = [422, certRefusal('cert_untrusted')];
  const w = atSignIn({ email: 'me@proton.me', password: 'bridge-pw' });
  w.choose('proton');
  expect(await w.next()).toBe(false);
  expect([w.test, w.cert, w.errorField, w.error]).toEqual(['err', cert, '', 'The certificate of 127.0.0.1 is not one this system trusts.']);

  routes['POST /api/accounts/test'] = [200, found];
  await w.acceptCert();
  expect([w.test, w.cert, w.certFingerprint]).toEqual(['ok', undefined, 'AB:CD']);
  w.password = 'bridge-pw-2';
  w.edited(); // the password is not the server: the accepted certificate stays
  await w.runTest();
  w.step = 3;
  expect(await w.next()).toBe(true);
  const sent = { preset: 'proton', username: 'me@proton.me', host: '127.0.0.1', port: 1143, tls_mode: 'starttls' };
  expect(bodies('/api/accounts/test')).toEqual([
    { ...sent, password: 'bridge-pw' },
    { ...sent, password: 'bridge-pw', cert_fingerprint: 'AB:CD' },
    { ...sent, password: 'bridge-pw-2', cert_fingerprint: 'AB:CD' },
  ]);
  expect(bodies('/api/accounts')).toEqual([{ ...sent, password: 'bridge-pw-2', cert_fingerprint: 'AB:CD' }]);
});

it.each<[string, (w: Wizard) => void]>([
  ['the host', (w) => w.serverEdited()],
  ['the port', (w) => w.portEdited()],
  ['the encryption', (w) => w.setTls('implicit')],
  ['the provider', (w) => w.choose('generic')],
])('forgets an accepted certificate when %s changes', async (_name, change) => {
  const w = atSignIn({ email: 'me@proton.me', password: 'bridge-pw' });
  w.choose('proton');
  w.certFingerprint = 'AB:CD';
  change(w);
  expect(w.certFingerprint).toBe('');
});

it('goes back to sign-in with the new certificate when the server presents another one at save', async () => {
  const w = atSignIn({ email: 'me@proton.me', password: 'bridge-pw' });
  w.choose('proton');
  w.certFingerprint = '00:11';
  await w.runTest();
  w.step = 3;
  routes['POST /api/accounts'] = [422, certRefusal('cert_changed')];
  expect(await w.next()).toBe(false);
  expect([w.step, w.test, w.cert]).toEqual([1, 'err', cert]);
});

it("sends a one-click tile to the provider's sign-in instead of the Sign in step", async () => {
  accounts.presets = [{ ...presets[0], name: 'gmail', label: 'Gmail', one_click_url: '/api/oauth/start?provider=gmail' }];
  const assign = vi.fn();
  vi.stubGlobal('location', { ...window.location, assign });
  const w = new Wizard();
  w.choose('gmail', 'one_click');
  expect(await w.next()).toBe(false);
  expect(assign).toHaveBeenCalledWith('/api/oauth/start?provider=gmail');
  expect(w.step).toBe(0);
  // Its app-password tile goes on to Sign in as before.
  w.choose('gmail');
  await w.next();
  expect(w.step).toBe(1);
  expect(assign).toHaveBeenCalledTimes(1);
});

it('goes on at Rules for a mailbox a one-click sign-in connected, and saves only the starter rules', async () => {
  accounts.list = [{ ...created, id: 7, label: 'jo@gmail.com', preset: 'gmail', one_click: true }];
  vi.mocked(addTemplatesByName).mockResolvedValue(2);
  const w = new Wizard(7);
  expect([w.step, w.first]).toEqual([2, 2]);
  await w.next();
  expect(w.nextLabel).toBe('Finish');
  expect(await w.next()).toBe(true);
  expect(bodies('/api/accounts')).toEqual([]);
  expect(bodies('/api/accounts/test')).toEqual([]);
  expect(vi.mocked(addTemplatesByName)).toHaveBeenCalledWith(['Newsletters', 'Receipts', 'Login codes']);
  expect(toast.text).toBe('jo@gmail.com is connected with 2 new rules');
});

describe("a module's sign-in form", () => {
  const form = '/api/proton/sign-in';
  beforeEach(() => {
    accounts.presets = [{ ...presets[2], password: false, form_url: form }];
    routes['GET /api/accounts'] = [200, { items: [{ ...created, id: 9, label: 'me@proton.me', preset: 'proton', tls_mode: 'starttls' }] }];
  });
  const answer = (r: FormStep) => (routes[`POST ${form}`] = [200, r]);

  it('asks for the password, a code and the mailbox password, then goes on at Rules', async () => {
    const w = new Wizard();
    w.choose('proton', 'form');
    await w.next();
    expect([w.step, w.formStep, w.nextLabel]).toEqual([1, 'sign_in', 'Sign in']);

    // Nothing typed: nothing is sent.
    await w.next();
    expect([w.formMessage, bodies(form)]).toEqual(['Enter your address and password first.', []]);

    // A refused password comes back as the module's sentence, on the same step, with the password cleared.
    answer({ step: 'sign_in', message: 'Proton refused that password.' });
    Object.assign(w, { email: ' me@proton.me ', password: 'wrong' });
    await w.next();
    expect([w.formStep, w.formMessage, w.password, w.email]).toEqual(['sign_in', 'Proton refused that password.', '', ' me@proton.me ']);

    answer({ step: 'code', state: 's1' });
    w.password = 'right';
    await w.next();
    expect([w.formStep, w.formMessage, w.nextLabel, w.password]).toEqual(['code', '', 'Continue', '']);

    answer({ step: 'code', state: 's1', message: 'That code is wrong.' });
    w.code = '000000';
    await w.submitForm();
    expect([w.formStep, w.formMessage, w.code]).toEqual(['code', 'That code is wrong.', '']);

    answer({ step: 'mailbox_password', state: 's1' });
    w.code = ' 424242 ';
    await w.submitForm();
    expect(w.formStep).toBe('mailbox_password');

    answer({ account_id: 9 });
    w.mailboxPassword = 'mbox';
    await w.submitForm();
    expect([w.step, w.first, w.accountId, w.test, w.mailboxPassword]).toEqual([2, 2, 9, 'ok', '']);
    expect(accounts.list.map((a) => a.id)).toEqual([9]);
    expect(bodies(form)).toEqual([
      { step: 'sign_in', username: 'me@proton.me', password: 'wrong' },
      { step: 'sign_in', username: 'me@proton.me', password: 'right' },
      { step: 'code', state: 's1', code: '000000' },
      { step: 'code', state: 's1', code: '424242' },
      { step: 'mailbox_password', state: 's1', password: 'mbox' },
    ]);

    // Then Preview and Finish save only the starter rules, as after a one-click sign-in.
    vi.mocked(addTemplatesByName).mockResolvedValue(3);
    await w.next();
    expect(w.nextLabel).toBe('Finish');
    expect(await w.next()).toBe(true);
    expect(fetchMock.mock.calls.filter((c) => c[1].method === 'POST' && c[0] === '/api/accounts')).toEqual([]);
    expect(toast.text).toBe('me@proton.me is connected with 3 new rules');
  });

  it("shows the daemon's refusal and starts over from a new tile", async () => {
    routes[`POST ${form}`] = [403, refusal('csrf_failed', 'Reload the page and try again.')];
    const w = new Wizard();
    w.choose('proton', 'form');
    await w.next();
    Object.assign(w, { email: 'me@proton.me', password: 'pw' });
    await w.next();
    expect([w.step, w.formMessage, w.password, w.busy]).toEqual([1, 'Reload the page and try again.', '', false]);
    answer({ step: 'code', state: 's2' });
    w.password = 'pw';
    await w.next();
    expect(w.formStep).toBe('code');
    w.choose('proton', 'form');
    expect([w.formStep, w.formMessage]).toEqual(['sign_in', '']);
  });
});
