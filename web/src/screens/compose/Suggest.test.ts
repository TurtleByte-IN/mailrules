import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi, type Mock } from 'vitest';
import type { RuleSuggestion, SuggestResult } from '../../lib/api/suggest';
import { startScope } from '../../lib/scope';
import { accounts } from '../../lib/state/accounts.svelte';
import { rules } from '../../lib/state/rules.svelte';
import { suggest } from '../../lib/state/suggest.svelte';
import { toast } from '../../lib/state/toast.svelte';
import Suggest from './Suggest.svelte';

type Reply = [number, unknown] | (() => Response);
type FetchMock = Mock<(url: string, init: RequestInit) => Promise<Response>>;

/** Answers each "METHOD /path" with its [status, body] or its own Response; anything else is a 404. */
function serve(routes: Record<string, Reply>): FetchMock {
  const fetchMock: FetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const r = routes[`${init.method} ${url}`] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
    if (typeof r === 'function') return r();
    return new Response(JSON.stringify(r[1]), { status: r[0], headers: { 'Content-Type': 'application/json' } });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const FOLDERS: Reply = [200, { items: [] }];
const sent = (f: FetchMock, path: string) => f.mock.calls.filter((c) => c[0] === path).map((c) => JSON.parse(c[1].body as string));

// A suggestion as POST /api/rules/suggest returns it.
const suggestion = (name: string, over: Partial<RuleSuggestion> = {}): RuleSuggestion => ({
  name,
  said: '',
  intent: null,
  account_id: 7,
  stack: false,
  model: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['acme.com'] }] },
  exceptions: {},
  actions: [{ type: 'archive' }],
  min_confidence: null,
  new_folders: [],
  question: null,
  conflicts: [],
  errors: [],
  match_count: 12,
  samples: [],
  kind: 'exact',
  trashes: false,
  reason: name + ' because the AI says so.',
  groups: [{ id: 'g1', from: 'news@acme.com', domain: 'acme.com', count: 12 }],
  ...over,
});

const row = (subject: string) => ({ from: 'x@spam.biz', subject, received_at: null, stage: 'none' as const, rule_id: null, rule_name: '', confidence: 0, reason: '', actions: [] });

const four = [
  suggestion('Acme news'),
  suggestion('Newsletters', { kind: 'meaning', intent: 'newsletters I never read', conditions: {}, match_count: 9, groups: Array.from({ length: 7 }, (_, i) => ({ id: 'g' + i, from: `n${i}@news.com`, domain: 'news.com', count: 7 - i })) }),
  suggestion('Cold pitches', { kind: 'meaning', intent: 'cold sales pitches', conditions: {}, actions: [{ type: 'trash' }], trashes: true, match_count: 4, samples: [row('Grow your pipeline'), row('Quick question')] }),
  suggestion('Broken', { errors: [{ path: 'actions[0].folder', message: 'The folder name is empty.' }], match_count: 0 }),
];

const result = (suggestions: RuleSuggestion[], over: Partial<SuggestResult> = {}): SuggestResult => ({
  suggestions,
  scanned: 40,
  total: 40,
  matched: 40,
  groups: 11,
  requests: 2,
  tokens: 3456,
  cost_usd: 0.0123,
  notes: ['Fewer samples were sent for 2 senders.'],
  ...over,
});

beforeEach(() => {
  Object.assign(suggest, {
    phase: 'idle',
    scope: startScope(),
    folders: [],
    samplesMode: 'upto',
    upTo: 5,
    body: 'none',
    samplesRefused: '',
    progress: null,
    result: null,
    cards: [],
    needsModel: '',
    created: 0,
  });
  Object.assign(rules, { list: [], loaded: true, error: '' });
  Object.assign(accounts, { list: [{ id: 7, label: 'me@icloud.com' }], loaded: true });
  toast.text = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

const note = () => screen.getByText(/^Sent to your AI model provider/).textContent;
// Its name changes while a scan runs.
const button = () => screen.getByRole<HTMLButtonElement>('button', { name: /^Suggest(ing…| rules)$/ });
const text = (el: HTMLElement) => el.textContent?.replace(/\s+/g, ' ').trim();

async function scanned(suggestions = four, extra: Record<string, Reply> = {}) {
  const f = serve({ 'GET /api/accounts/7/folders': FOLDERS, 'POST /api/rules/suggest': [200, result(suggestions)], ...extra });
  render(Suggest);
  await fireEvent.click(button());
  await screen.findByRole('heading', { level: 2 });
  return f;
}

it('says what leaves the server for each choice, and refuses a samples box that is not a whole number', async () => {
  serve({ 'GET /api/accounts/7/folders': FOLDERS });
  render(Suggest);
  const base = "Sent to your AI model provider: each sender's address, name and email counts, and the subject and date of ";
  expect(screen.getByText(/^MailRules reads the emails you pick/)).toBeTruthy();
  expect(note()).toBe(base + 'up to 5 emails per sender. No email text.');

  await fireEvent.change(screen.getByLabelText('Body sent to the AI'), { target: { value: 'first500' } });
  expect(note()).toBe(base + 'up to 5 emails per sender plus the first 500 characters of each of those emails.');
  await fireEvent.change(screen.getByLabelText('Body sent to the AI'), { target: { value: 'full' } });
  expect(note()).toBe(base + 'up to 5 emails per sender plus the full text of each of those emails.');

  await fireEvent.input(screen.getByLabelText('How many samples per sender'), { target: { value: '1' } });
  expect(note()).toBe(base + 'up to 1 email per sender plus the full text of each of those emails.');
  await fireEvent.change(screen.getByLabelText('Samples per sender'), { target: { value: 'all' } });
  expect(note()).toBe(base + 'all emails per sender plus the full text of each of those emails.');
  expect(screen.queryByLabelText('How many samples per sender')).toBeNull();

  await fireEvent.change(screen.getByLabelText('Samples per sender'), { target: { value: 'upto' } });
  await fireEvent.input(screen.getByLabelText('How many samples per sender'), { target: { value: '0' } });
  expect(screen.getByRole('alert').textContent).toBe('Give a whole number of samples, 1 or more.');
  expect(screen.getByLabelText('How many samples per sender').getAttribute('aria-describedby')).toBe('suggest-samples-problem');
  expect(button().disabled).toBe(true);
});

it('offers the same scope as Cleanup and refuses a wrong number there too', async () => {
  serve({ 'GET /api/accounts/7/folders': [200, { items: [{ name: 'Archive', special_use: '\\Archive' }] }] });
  render(Suggest);
  expect([...screen.getByLabelText<HTMLSelectElement>('Which emails').options].map((o) => o.text)).toEqual(['Newest emails', 'From the last days', 'All mail']);
  await vi.waitFor(() => expect([...screen.getByLabelText<HTMLSelectElement>('Folder').options].map((o) => o.text)).toEqual(['Inbox', 'Archive']));
  await fireEvent.input(screen.getByLabelText('How many emails'), { target: { value: '0' } });
  expect(screen.getByRole('alert').textContent).toBe('The limit must be between 1 and 2000.');
  expect(screen.getByLabelText('How many emails').getAttribute('aria-describedby')).toBe('suggest-scope-problem');
  expect(button().disabled).toBe(true);
});

it.each([
  ['the newest emails, up to 5 samples, no body', [], { folder: 'INBOX', since: null, limit: 200, samples: 5, body: 'none' }],
  [
    'the last 30 days, every sample, the full body',
    [['Which emails', 'days', 'change'], ['How many days', '30', 'input'], ['Samples per sender', 'all', 'change'], ['Body sent to the AI', 'full', 'change']],
    { folder: 'INBOX', since: 1790000000 - 30 * 86400, limit: 2000, samples: 'all', body: 'full' },
  ],
  ['all mail, 12 samples, 500 characters', [['Which emails', 'all', 'change'], ['How many samples per sender', '12', 'input'], ['Body sent to the AI', 'first500', 'change']], { folder: 'INBOX', since: null, limit: 2000, samples: 12, body: 'first500' }],
] as const)('asks for %s', async (_name, steps, request) => {
  vi.spyOn(Date, 'now').mockReturnValue(1790000000 * 1000);
  const f = serve({ 'GET /api/accounts/7/folders': FOLDERS, 'POST /api/rules/suggest': [200, result([])] });
  render(Suggest);
  for (const [label, value, how] of steps) await fireEvent[how](screen.getByLabelText(label), { target: { value } });
  await fireEvent.click(button());
  expect(await screen.findByText('The AI found nothing worth a rule in these emails.')).toBeTruthy();
  expect(sent(f, '/api/rules/suggest')).toEqual([{ account_id: 7, ...request }]);
  const init = f.mock.calls.find((c) => c[0] === '/api/rules/suggest')![1];
  expect(init.headers).toMatchObject({ Accept: 'application/json, text/event-stream' });
});

it('shows how far the scan is in each phase, with its tokens and cost', async () => {
  let ctl!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({ start: (c) => void (ctl = c) });
  const send = (event: string, data: unknown) => ctl.enqueue(new TextEncoder().encode(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`));
  serve({ 'GET /api/accounts/7/folders': FOLDERS, 'POST /api/rules/suggest': () => new Response(body, { headers: { 'Content-Type': 'text/event-stream' } }) });
  render(Suggest);
  await fireEvent.click(button());
  expect(await screen.findByText('Reading your mailbox…')).toBeTruthy();
  expect(button().textContent?.trim()).toBe('Suggesting…');
  expect(button().disabled).toBe(true);

  const p = { read: 0, total: 2000, matched: 5000, requests_done: 0, requests_total: 0, tokens: 0, cost_usd: 0 };
  send('progress', { ...p, phase: 'reading', read: 500 });
  expect(await screen.findByText('500 of 2,000 emails read · 0 tokens · $0.00')).toBeTruthy();
  expect(screen.getByRole('progressbar', { name: 'Scan progress' }).getAttribute('aria-valuenow')).toBe('25');
  expect(screen.getByText('Scanning the newest 2,000 of 5,000.')).toBeTruthy();

  send('progress', { ...p, phase: 'asking', read: 2000, requests_total: 3, tokens: 1500, cost_usd: 0.005 });
  expect(await screen.findByText('Asking the AI: request 1 of 3 · 1,500 tokens · $0.0050')).toBeTruthy();
  expect(screen.queryByRole('progressbar')).toBeNull();

  send('progress', { ...p, phase: 'merging', read: 2000, requests_done: 2, requests_total: 3, tokens: 4200, cost_usd: 0.0123 });
  expect(await screen.findByText('Merging the suggestions · 4,200 tokens · $0.01')).toBeTruthy();

  send('done', result(four.slice(0, 1), { scanned: 2000, total: 2000, matched: 5000 }));
  ctl.close();
  expect(await screen.findByRole('heading', { name: '1 suggestion from 2,000 emails' })).toBeTruthy();
  expect(button().textContent?.trim()).toBe('Suggest rules');
});

it('shows each card with its kind, preview and senders; trash starts unticked and an error card cannot be ticked', async () => {
  await scanned();
  expect(screen.getByRole('heading', { name: '4 suggestions from 40 emails' })).toBeTruthy();
  expect(screen.getByText('11 senders · 2 AI requests · 3,456 tokens · $0.01')).toBeTruthy();
  expect(screen.getByRole('status').textContent).toContain('Fewer samples were sent for 2 senders.');

  const tick = (name: string) => screen.getByRole<HTMLInputElement>('checkbox', { name: 'Create ' + name });
  expect(tick('Acme news').checked).toBe(true);
  expect(tick('Newsletters').checked).toBe(true);
  expect(tick('Cold pitches').checked).toBe(false);
  expect(tick('Broken').checked).toBe(false);
  expect(tick('Broken').disabled).toBe(true);
  expect(screen.getByText('Cannot be created as it is')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Create 2 selected' })).toBeTruthy();

  const [acme, news, cold] = screen.getAllByRole('article');
  expect(acme.textContent).toContain('Exact · no AI needed');
  expect(acme.textContent).toContain('Acme news because the AI says so.');
  expect(acme.textContent).toContain('Matches 12 of the 40 scanned emails');
  expect(acme.textContent).toContain('From: news@acme.com (12)');
  expect(news.textContent).toContain('By meaning · AI decides each email');
  expect(news.textContent).toContain('The AI grouped 9 of the 40 scanned emails here (its own guess: a rule by meaning is decided by the AI on each new email)');
  expect(news.textContent).toContain('From: n0@news.com (7), n1@news.com (6), n2@news.com (5), n3@news.com (4), n4@news.com (3) +2 more');
  expect(cold.textContent).toContain('Trash');
  expect(cold.textContent).toContain('Trashes 4 of the scanned emails');
  expect(cold.textContent).toContain('Grouped here: Grow your pipeline; Quick question');
  expect(cold.className).toContain('border-trash');

  await fireEvent.click(tick('Acme news'));
  expect(screen.getByRole('button', { name: 'Create 1 selected' })).toBeTruthy();
  await fireEvent.click(tick('Newsletters'));
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Create 0 selected' }).disabled).toBe(true);
});

it('creates exactly the ticked cards as rules, then points at Cleanup for the mail already there', async () => {
  const added = (name: string, id: number) => ({ ...suggestion(name), id });
  const f = await scanned(four, { 'POST /api/rules/batch': [201, { items: [added('Acme news', 1), added('Newsletters', 2), added('Cold pitches', 3)] }] });
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Create Cold pitches' }));
  await fireEvent.input(screen.getAllByLabelText('Rule name')[0], { target: { value: 'Acme updates' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Create 3 selected' }));

  const status = await screen.findByText(/^Created 3 rules/);
  expect(text(status)).toBe('Created 3 rules. They sort new mail from now on; the emails already in your mailbox stay where they are.');
  expect(status.getAttribute('role')).toBe('status');
  expect(screen.getByRole('link', { name: 'Sort existing mail in Cleanup' }).getAttribute('href')).toBe('#/cleanup');

  const [{ rules: body }] = sent(f, '/api/rules/batch');
  expect(body.map((r: RuleSuggestion) => r.name)).toEqual(['Acme updates', 'Newsletters', 'Cold pitches']);
  for (const r of body) expect(Object.keys(r).sort()).toEqual(['account_id', 'actions', 'conditions', 'exceptions', 'intent', 'min_confidence', 'model', 'name', 'new_folders', 'said', 'stack']);
  expect(body[2].actions).toEqual([{ type: 'trash' }]);

  await fireEvent.click(screen.getByRole('button', { name: 'Suggest again' }));
  expect(screen.queryByText(/^Created/)).toBeNull();
  expect(button().disabled).toBe(false);
});

it('says so in the singular after creating one rule', async () => {
  await scanned(four.slice(0, 1), { 'POST /api/rules/batch': [201, { items: [{ ...suggestion('Acme news'), id: 1 }] }] });
  await fireEvent.click(screen.getByRole('button', { name: 'Create 1 selected' }));
  expect(text(await screen.findByText(/^Created 1 rule\./))).toBe(
    'Created 1 rule. It sorts new mail from now on; the emails already in your mailbox stay where they are.',
  );
});

it('puts a refused create on the card the daemon names, counting only the cards sent', async () => {
  const message = 'There is already a rule named "Newsletters". Choose another name.';
  const f = await scanned(four, { 'POST /api/rules/batch': [400, { error: { code: 'rule_invalid', message, path: 'rules[0].name' } }] });
  // With the first card unticked, the daemon's rules[0] is the second card.
  await fireEvent.click(screen.getByRole('checkbox', { name: 'Create Acme news' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Create 1 selected' }));

  const alert = await screen.findByText(message);
  expect(screen.getAllByRole('article')[1].contains(alert)).toBe(true);
  expect(screen.getAllByLabelText('Rule name').map((n) => n.getAttribute('aria-invalid'))).toEqual([null, 'true', null, null]);
  expect(sent(f, '/api/rules/batch')[0].rules.map((r: RuleSuggestion) => r.name)).toEqual(['Newsletters']);
  expect(toast.text).toBe('');
});

it('says a model is needed and links to Settings', async () => {
  const message = 'Choose a model for drafting rules in Settings first.';
  serve({ 'GET /api/accounts/7/folders': FOLDERS, 'POST /api/rules/suggest': [409, { error: { code: 'no_composer_model', message } }] });
  render(Suggest);
  await fireEvent.click(button());
  const alert = await screen.findByText(message, { exact: false });
  expect(alert.textContent).toBe(message + ' Open Settings');
  expect(screen.getByRole('link', { name: 'Open Settings' }).getAttribute('href')).toBe('#/settings');
});

it('says the Claude workspace is needed and links to Settings', async () => {
  const message = 'Your Claude key covers your whole organisation, so MailRules needs to know which workspace to use. Choose it in Settings.';
  serve({ 'GET /api/accounts/7/folders': FOLDERS, 'POST /api/rules/suggest': [409, { error: { code: 'anthropic_workspace_needed', message } }] });
  render(Suggest);
  await fireEvent.click(button());
  expect((await screen.findByText(message, { exact: false })).textContent).toBe(message + ' Open Settings');
  expect(screen.getByRole('link', { name: 'Open Settings' }).getAttribute('href')).toBe('#/settings');
});

it('offers the mailbox a rule applies to when more than one is connected', async () => {
  Object.assign(accounts, { list: [{ id: 7, label: 'me@icloud.com' }, { id: 8, label: 'work@acme.com' }] });
  const f = await scanned(four.slice(0, 1), { 'POST /api/rules/batch': [201, { items: [{ ...suggestion('Acme news'), id: 1 }] }] });
  const applies = screen.getByLabelText<HTMLSelectElement>('Applies to');
  expect(applies.value).toBe('7');
  await fireEvent.change(applies, { target: { value: '' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Create 1 selected' }));
  await screen.findByText(/^Created 1 rule/);
  expect(sent(f, '/api/rules/batch')[0].rules[0].account_id).toBeNull();
});
