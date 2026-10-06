import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Draft as ApiDraft } from '../api/compose';
import type { Rule } from '../api/rules';
import type { Template } from '../api/templates';
import { addTemplate, addTemplatesByName, compose, optimize, saveAll } from './compose.svelte';
import { rules } from './rules.svelte';
import { toast } from './toast.svelte';

// A draft as POST /api/rules/compose returns it.
const draft = (name: string, over: Partial<ApiDraft> = {}): ApiDraft => ({
  name,
  said: name + ' in my words',
  intent: null,
  account_id: null,
  stack: false,
  model: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['linkedin.com'] }] },
  exceptions: {},
  actions: [{ type: 'archive' }],
  min_confidence: null,
  new_folders: [],
  question: null,
  conflicts: [],
  errors: [],
  match_count: 6,
  samples: [],
  ...over,
});

// A rule as the daemon returns it once saved.
const saved = (id: number, name: string) =>
  ({ id, name, account_id: null, said: '', intent: '', conditions: {}, exceptions: {}, actions: [{ type: 'archive' }], priority: id, stack: false, model: '', min_confidence: null, enabled: true, version: 1, created_at: 1791276732, updated_at: 1791276732, hits_week: 0, last_match_at: null }) satisfies Rule;

const template = (name: string): Template => ({ id: name.toLowerCase(), name, description: name + ' you rarely open', rule: { name, intent: name, actions: [{ type: 'move', folder: name }], new_folders: [name], stack: false, enabled: true } });

/** Answers each "METHOD /path" with its [status, body]; anything else is a 404. */
function serve(routes: Record<string, [number, unknown?]>) {
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[`${init.method} ${url}`] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const sent = (f: ReturnType<typeof serve>, call = 0) => JSON.parse(f.mock.calls[call][1].body as string);

beforeEach(() => {
  Object.assign(compose, { text: '', drafts: [], unparsed: [], busy: false, templates: [], needsModel: '' });
  Object.assign(rules, { list: [saved(1, 'Food')], loaded: true, error: '' });
  toast.text = '';
});
afterEach(() => vi.unstubAllGlobals());

it('turns text into drafts and keeps what could not be parsed', async () => {
  const f = serve({ 'POST /api/rules/compose': [200, { rules: [draft('Finance'), draft('LinkedIn')], unparsed: ['and the other stuff'] }] });
  compose.text = '  Bank statements go to Finance. Archive LinkedIn. And the other stuff  ';
  await optimize();

  expect(sent(f)).toEqual({ text: 'Bank statements go to Finance. Archive LinkedIn. And the other stuff' });
  expect(compose.drafts.map((d) => [d.name, d.rejected])).toEqual([['Finance', false], ['LinkedIn', false]]);
  expect(compose).toMatchObject({ unparsed: ['and the other stuff'], busy: false, needsModel: '' });
});

it.each<[string, unknown, number, string]>([
  ['', undefined, 0, 'Type or dictate at least one rule first'],
  ['ok', { rules: [], unparsed: ['ok'] }, 1, "Couldn't find a rule in that. Try describing which emails and where they go."],
])('optimize(%j) flashes why there are no drafts', async (text, body, calls, flashed) => {
  const f = serve({ 'POST /api/rules/compose': [200, body] });
  compose.text = text;
  await optimize();
  expect(f).toHaveBeenCalledTimes(calls);
  expect(compose.drafts).toEqual([]);
  expect(toast.text).toBe(flashed);
});

it('saves only the drafts not skipped and not in error, as rule inputs', async () => {
  const f = serve({ 'POST /api/rules/batch': [201, { items: [saved(2, 'Finance'), saved(3, 'Cold sales')] }] });
  compose.text = 'typed';
  compose.drafts = [
    { ...draft('Finance', { intent: 'Bank statements', actions: [{ type: 'move', folder: 'Finance' }], new_folders: ['Finance'] }), rejected: false },
    { ...draft('LinkedIn'), rejected: true },
    { ...draft('Broken', { errors: [{ path: 'actions', message: 'A rule needs at least one action.' }] }), rejected: false },
    { ...draft('Cold sales', { intent: 'Cold sales pitches', actions: [{ type: 'trash' }], question: 'Trash them, or keep them in a Sales folder?' }), rejected: false },
  ];
  const added = await saveAll();

  expect(sent(f).rules).toEqual([
    { name: 'Finance', said: 'Finance in my words', intent: 'Bank statements', conditions: draft('x').conditions, exceptions: {}, actions: [{ type: 'move', folder: 'Finance' }], min_confidence: null, new_folders: ['Finance'], stack: false, enabled: true },
    { name: 'Cold sales', said: 'Cold sales in my words', intent: 'Cold sales pitches', conditions: draft('x').conditions, exceptions: {}, actions: [{ type: 'trash' }], min_confidence: 0.9, new_folders: [], stack: false, enabled: true },
  ]);
  expect(added?.map((r) => r.id)).toEqual([2, 3]);
  expect(rules.list.map((r) => r.id)).toEqual([1, 2, 3]);
  expect(compose).toMatchObject({ drafts: [], text: '' });
  expect(toast.text).toBe('2 rules saved and live');
});

it('saves nothing when every draft is skipped', async () => {
  const f = serve({});
  compose.drafts = [{ ...draft('Finance'), rejected: true }];
  expect(await saveAll()).toBeUndefined();
  expect(f).not.toHaveBeenCalled();
  expect(compose.drafts).toHaveLength(1);
  expect(toast.text).toBe('Nothing to save: every draft is skipped');
});

const noModel = 'This needs an AI model, and none is set up yet. Add a Claude (Anthropic) key in Settings, then try again. Rules built from conditions work without one.';

it('keeps the daemon\'s sentence when there is no model to compose with, and what was typed', async () => {
  serve({ 'POST /api/rules/compose': [409, { error: { code: 'no_composer_model', message: noModel } }] });
  compose.text = 'Archive LinkedIn';
  await optimize();
  expect(compose).toMatchObject({ needsModel: noModel, text: 'Archive LinkedIn', busy: false });
  expect(toast.text).toBe('');

  // It goes when composing works.
  serve({ 'POST /api/rules/compose': [200, { rules: [draft('LinkedIn')], unparsed: [] }] });
  await optimize();
  expect(compose.needsModel).toBe('');
});

it.each<[string, string, () => Promise<unknown>]>([
  ['model_error', 'POST /api/rules/compose', optimize],
  ['mailbox_error', 'POST /api/rules/batch', saveAll],
])('flashes a 502 %s and keeps the text and drafts', async (code, route, action) => {
  serve({ [route]: [502, { error: { code, message: 'The other end did not answer.' } }] });
  compose.text = 'Archive LinkedIn';
  compose.drafts = [{ ...draft('LinkedIn'), rejected: false }];
  await action();

  expect(compose).toMatchObject({ needsModel: '', text: 'Archive LinkedIn', busy: false });
  expect(compose.drafts).toHaveLength(1);
  expect(rules.list).toHaveLength(1);
  expect(toast.text).toBe('The other end did not answer.');
});

it('adds the named templates as rules, skipping names already taken', async () => {
  const f = serve({
    'GET /api/templates': [200, { items: [template('Food'), template('Receipts'), template('Travel')] }],
    'POST /api/rules/batch': [201, { items: [saved(2, 'Receipts')] }],
  });
  expect(await addTemplatesByName(['Food', 'Receipts', 'Nope'])).toBe(1);
  expect(sent(f, 1)).toEqual({ rules: [template('Receipts').rule] });
  expect(rules.list.map((r) => r.name)).toEqual(['Food', 'Receipts']);

  // The gallery is fetched once; nothing new to add asks for nothing.
  expect(await addTemplatesByName(['Food', 'Receipts'])).toBe(0);
  expect(f).toHaveBeenCalledTimes(2);
});

it('lets addTemplatesByName fail to its caller, and flashes the failure from the gallery button', async () => {
  const refused = { error: { code: 'mailbox_error', message: 'The folder Travel could not be created. Nothing was saved.' } };
  serve({ 'GET /api/templates': [200, { items: [template('Travel')] }], 'POST /api/rules/batch': [502, refused] });
  await expect(addTemplatesByName(['Travel'])).rejects.toMatchObject({ status: 502, code: 'mailbox_error' });
  expect(toast.text).toBe('');

  await addTemplate(compose.templates[0]);
  expect(toast.text).toBe(refused.error.message);
});
