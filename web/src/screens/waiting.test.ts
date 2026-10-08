// What the screens show while the daemon is being waited on: the rule test's live count and the
// waiting states of every other action that takes more than a moment.
import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Draft } from '../lib/api/compose';
import { dispatch } from '../lib/api/events';
import type { Rule } from '../lib/api/rules';
import { accounts } from '../lib/state/accounts.svelte';
import { cleanup as cleanupState } from '../lib/state/cleanup.svelte';
import { compose } from '../lib/state/compose.svelte';
import { rules } from '../lib/state/rules.svelte';
import { senders } from '../lib/state/senders.svelte';
import { testLimit } from '../lib/state/testlimit.svelte';
import Wizard from './accounts/Wizard.svelte';
import Build from './compose/Build.svelte';
import Cleanup from './Cleanup.svelte';
import Compose from './Compose.svelte';
import Rules from './Rules.svelte';
import Senders from './Senders.svelte';

const food: Rule = {
  id: 7,
  account_id: null,
  name: 'Food',
  said: 'Swiggy goes to Food',
  template: '',
  intent: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
  exceptions: {},
  actions: [{ type: 'move', folder: 'Food' }],
  priority: 1,
  stack: false,
  model: '',
  min_confidence: null,
  enabled: true,
  version: 1,
  created_at: 1791276732,
  updated_at: 1791276732,
  hits_week: 3,
  last_match_at: null,
};
const mailbox = { id: 3, label: 'me@icloud.com' } as (typeof accounts.list)[number];
const done = { results: [], tested: 20, matched: 4, model_calls: 2, cost_usd: 0.00004 };

type Call = { method: string; url: string; body: unknown };
type Answer = [status: number, body: unknown];
/**
 * Answers each "METHOD /path" with [status, body]. A route set to 'hold' is left unanswered until
 * `release` gives its answer, which is how a test looks at the screen while the daemon is busy.
 * A route set to 'stream' answers with an event stream the test writes to with `emit`.
 */
function serve(routes: Record<string, Answer | 'hold' | 'stream'>) {
  const calls: Call[] = [];
  const held: Record<string, (a: Answer) => void> = {};
  let stream!: ReadableStreamDefaultController<Uint8Array>;
  const enc = new TextEncoder();
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const key = `${init.method} ${url}`;
      calls.push({ method: init.method!, url, body: init.body && typeof init.body === 'string' ? JSON.parse(init.body) : init.body });
      const route = routes[key] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
      if (route === 'stream') {
        return new Response(new ReadableStream({ start: (c) => void (stream = c) }), { status: 200, headers: { 'Content-Type': 'text/event-stream' } });
      }
      if (route === 'hold') return new Promise<Response>((resolve) => (held[key] = ([status, body]) => resolve(new Response(JSON.stringify(body), { status }))));
      return new Response(JSON.stringify(route[1]), { status: route[0] });
    }),
  );
  return {
    calls,
    release: (key: string, answer: Answer) => held[key](answer),
    emit: (event: string, data: unknown) => stream.enqueue(enc.encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`)),
    end: () => stream.close(),
  };
}

beforeEach(() => {
  testLimit.value = 200;
  Object.assign(rules, { list: [food], loaded: true, error: '' });
  Object.assign(accounts, { list: [mailbox], loaded: true });
  Object.assign(compose, { text: '', drafts: [], unparsed: [], busy: false, templates: [], needsModel: '' });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('Rules: tests the number of emails chosen and shows N of M as the daemon reports it', async () => {
  const d = serve({ 'POST /api/rules/test': 'stream' });
  render(Rules);
  const count = screen.getByRole<HTMLInputElement>('spinbutton', { name: 'How many emails to test' });
  expect(count.value).toBe('200');
  await fireEvent.input(count, { target: { value: '20' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 20 emails' }));

  expect(d.calls.at(-1)).toEqual({ method: 'POST', url: '/api/rules/test', body: { account_id: 3, rule_ids: [7], limit: 20 } });
  expect(screen.getByRole('button', { name: 'Testing on last 20 emails…' })).toBeTruthy();
  d.emit('progress', { done: 0, total: 20, model_calls: 0 });
  expect(await screen.findByText('0 of 20 emails tested')).toBeTruthy();
  d.emit('progress', { done: 14, total: 20, model_calls: 6 });
  expect(await screen.findByText('14 of 20 emails tested · 6 sent to the decision model')).toBeTruthy();
  d.emit('done', done);

  expect(await screen.findByText(/Matched 4 of your last 20 emails/)).toBeTruthy();
  expect(screen.queryByRole('progressbar')).toBeNull();
  // The number stays for the next test.
  expect(screen.getByRole<HTMLInputElement>('spinbutton', { name: 'How many emails to test' }).value).toBe('20');
  expect(testLimit.value).toBe(20);
});

it("Rules: the daemon's refusal of the number is shown beside the number, not as a toast", async () => {
  serve({ 'POST /api/rules/test': [400, { error: { code: 'invalid_input', message: 'The limit must be between 1 and 2000.', path: 'limit' } }] });
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  expect((await screen.findByRole('alert')).textContent).toBe('The limit must be between 1 and 2000.');
  expect(screen.getByRole('spinbutton', { name: 'How many emails to test' }).getAttribute('aria-invalid')).toBe('true');
});

it('Rules: a refused test (no model) still reads out where the result goes, and the box stays usable', async () => {
  serve({ 'POST /api/rules/test': [409, { error: { code: 'no_composer_model', message: 'This needs an AI model.' } }] });
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  expect((await screen.findByRole('alert')).textContent).toBe('This needs an AI model.');
  expect(screen.getByRole<HTMLInputElement>('spinbutton', { name: 'How many emails to test' }).disabled).toBe(false);
});

it('Rules: a test Claude refused for want of a workspace says so where the result goes', async () => {
  const message = 'Your Claude key covers your whole organisation, so MailRules needs to know which workspace to use. Choose it in Settings.';
  serve({ 'POST /api/rules/test': [409, { error: { code: 'anthropic_workspace_needed', message } }] });
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  expect((await screen.findByRole('alert')).textContent).toBe(message);
});

it('Build: tests a draft on the number chosen, from the same remembered number', async () => {
  testLimit.value = 50;
  const d = serve({ 'POST /api/rules/test': [200, { ...done, tested: 50 }] });
  render(Build, { rule: food });
  expect(screen.getByRole<HTMLInputElement>('spinbutton', { name: 'How many emails to test' }).value).toBe('50');
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 50 emails' }));
  expect(await screen.findByText(/Matched 4 of your last 50 emails/)).toBeTruthy();
  expect(d.calls[0].body).toMatchObject({ account_id: 3, limit: 50, rules: [{ name: 'Food', enabled: true }] });
});

it('Rules: importing a file says what is happening, and the control is busy until it answers', async () => {
  const d = serve({ 'POST /api/rules/import': 'hold' });
  const { container } = render(Rules);
  const file = new File(['rules: []'], 'rules.yaml');
  await fireEvent.change(container.querySelector('input[type=file]')!, { target: { files: [file] } });

  expect(screen.getByText('Checking and saving your rules file…')).toBeTruthy();
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Importing…' }).disabled).toBe(true);
  d.release('POST /api/rules/import', [200, { items: [food], created: 0, updated: 1 }]);
  expect(await screen.findByText('Imported rules.yaml: 0 added, 1 updated.')).toBeTruthy();
  expect(screen.queryByText('Checking and saving your rules file…')).toBeNull();
  expect(screen.getByRole('button', { name: 'Import rules' })).toBeTruthy();
});

it('Rules: undoing what a rule did today is busy until the daemon is through', async () => {
  const d = serve({ 'POST /api/rules/7/undo?since=1': 'hold' });
  vi.spyOn(Date.prototype, 'setHours').mockReturnValue(1000);
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Undo what it did today' }));
  // It moves real mail, dry-run or not, so it asks first and sends nothing until confirmed.
  expect(screen.getByRole('alertdialog', { name: new RegExp(`every email ${food.name} moved today`) }).textContent).toContain('Dry-run does not stop an undo');
  expect(d.calls.some((c) => c.url.includes('/undo'))).toBe(false);
  await fireEvent.click(screen.getByRole('button', { name: 'Yes, undo today' }));
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Undoing…' }).disabled).toBe(true);
  expect(screen.getByText('Putting the emails back where they were…')).toBeTruthy();
  d.release('POST /api/rules/7/undo?since=1', [200, { batch: {}, undone: 2, failed: 0 }]);
  expect(await screen.findByRole('button', { name: 'Undo what it did today' })).toBeTruthy();
});

it('Cleanup: a running check shows a waiting state until it is ready', async () => {
  Object.assign(cleanupState, { phase: 'idle', check: null, excluded: new Set(), batch: null, batches: [], next: null, status: 'ready', scope: { accountId: '3', folder: 'INBOX', mode: 'newest', newest: 200, days: 90 }, folders: [] });
  const base = { id: 'c1', account_id: 3, folder: 'INBOX', since: null, limit: 2000, done: 0, total: 0, matched: 0, model_calls: 0, tokens: 0, cost_usd: 0, error: '', rows: [], exclude: [] };
  let current: unknown = null;
  let releaseCheck: (() => void) | null = null;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const key = `${init.method} ${url}`;
      if (key === 'GET /api/cleanup/check?account_id=3') return new Response(JSON.stringify({ check: current }), { status: 200 });
      if (key === 'POST /api/cleanup/check') return new Promise<Response>((resolve) => (releaseCheck = () => resolve(new Response(JSON.stringify({ check: { ...base, status: 'running' } }), { status: 202 }))));
      const body = key === 'GET /api/batches?kind=cleanup' ? { items: [], next_cursor: null } : key === 'GET /api/accounts/3/folders' ? { items: [] } : { error: { code: 'not_found', message: 'no route' } };
      return new Response(JSON.stringify(body), { status: key.startsWith('GET') ? 200 : 404 });
    }),
  );
  render(Cleanup);
  await fireEvent.click(await screen.findByRole('button', { name: 'Check what would move' }));

  expect(screen.getByText('Reading your mailbox and checking each email against your rules…')).toBeTruthy();
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Checking…' }).disabled).toBe(true);

  releaseCheck!();
  // The check turns ready; the event carries no rows, so the screen GETs them, ending the wait.
  current = { ...base, status: 'ready' };
  dispatch('check.progress', { ...base, status: 'ready' });
  expect(await screen.findByRole('button', { name: 'Check what would move' })).toBeTruthy();
  expect(screen.queryByText(/checking each email/)).toBeNull();
});

it('Compose: turning text into rules says the AI model is being asked', async () => {
  const d = serve({ 'POST /api/rules/compose': 'hold' });
  render(Compose);
  await fireEvent.input(screen.getByLabelText('What should happen to your email?'), { target: { value: 'Swiggy to Food' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Turn into rules' }));

  expect(screen.getByText('Asking the AI model to draft your rules…')).toBeTruthy();
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Tidying up your rules…' }).disabled).toBe(true);
  const draft = { name: 'Food', said: 'Swiggy to Food', intent: null, conditions: food.conditions, exceptions: {}, actions: food.actions, account_id: null, stack: false, model: '', min_confidence: null, new_folders: [], question: null, conflicts: [], errors: [], match_count: 1, tested: 200, samples: [] } satisfies Draft;
  d.release('POST /api/rules/compose', [200, { rules: [draft], unparsed: [] }]);
  expect(await screen.findByText('1 rule found. Check them before saving.')).toBeTruthy();
  expect(screen.queryByText('Asking the AI model to draft your rules…')).toBeNull();
});

it('Rewrite: says the AI model is being asked', async () => {
  const d = serve({ 'POST /api/rules/7/compose': 'hold' });
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Rewrite with AI' }));
  await fireEvent.input(screen.getByLabelText('What should change about this rule?'), { target: { value: 'also skip my bank' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Rewrite rule' }));
  expect(screen.getByText('Asking the AI model to rewrite this rule…')).toBeTruthy();
  d.release('POST /api/rules/7/compose', [409, { error: { code: 'no_composer_model', message: 'none' } }]);
  expect(await screen.findByText(/none/)).toBeTruthy();
  expect(screen.queryByText('Asking the AI model to rewrite this rule…')).toBeNull();
});

it('Senders: the list says it is loading', async () => {
  Object.assign(senders, { status: 'loading', error: '', list: [], next: null, learned: [], sort: 'volume', q: '' });
  const d = serve({ 'GET /api/senders?sort=volume': 'hold', 'GET /api/senders?source=learned&limit=100': [200, { items: [], next_cursor: null }] });
  render(Senders);
  expect(screen.getByText('Loading your senders…')).toBeTruthy();
  d.release('GET /api/senders?sort=volume', [200, { items: [], next_cursor: null }]);
  expect(await screen.findByText('No senders found.')).toBeTruthy();
  expect(screen.queryByText('Loading your senders…')).toBeNull();
});

it('Wizard: testing the connection says what the daemon is doing, with the seconds once it takes a while', async () => {
  vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] });
  try {
    Object.assign(accounts, {
      list: [],
      presets: [{ name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: true, secret_label: 'App-specific password' }],
    });
    const d = serve({ 'POST /api/accounts/test': 'hold' });
    const { container } = render(Wizard, { onclose: () => {} });
    await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
    await fireEvent.input(screen.getByLabelText('iCloud Mail email address'), { target: { value: 'me@icloud.com' } });
    await fireEvent.input(screen.getByLabelText('App-specific password'), { target: { value: 'abcd-efgh-ijkl-mnop' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Test connection' }));

    const waiting = screen.getByText('Logging in to your mail server and listing its folders…');
    expect(waiting).toBeTruthy();
    await vi.advanceTimersByTimeAsync(9000);
    expect(within(waiting.parentElement!).getByText('9s')).toBeTruthy();
    expect(container.textContent).toContain('Testing connection…');
    d.release('POST /api/accounts/test', [200, { username: 'me', folders: [], can_move: true, idle: true }]);
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText('Logging in to your mail server and listing its folders…')).toBeNull();
  } finally {
    vi.useRealTimers();
  }
});
