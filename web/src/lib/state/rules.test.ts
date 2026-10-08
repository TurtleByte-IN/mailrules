import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Rule } from '../api/rules';
import { add, edit, importFile, load, move, moved, remove, rules, undoToday } from './rules.svelte';
import { toast } from './toast.svelte';

describe('moved', () => {
  const list = ['a', 'b', 'c', 'd'];
  it.each([
    ['up one', 2, 1, 'acbd'],
    ['down one', 0, 1, 'bacd'],
    ['dragged to the top', 3, 0, 'dabc'],
    ['dragged to the bottom', 0, 3, 'bcda'],
    ['onto itself', 1, 1, 'abcd'],
    ['first one up', 0, -1, 'abcd'],
    ['last one down', 3, 4, 'abcd'],
    ['unknown item', -1, 2, 'abcd'],
  ])('%s', (_, from, to, want) => {
    expect(moved(list, from, to).join('')).toBe(want);
    expect(list.join('')).toBe('abcd');
  });
});

// A rule as GET /api/rules returns it.
const rule = (id: number, over: Partial<Rule> = {}): Rule => ({
  id,
  account_id: null,
  name: 'Rule ' + id,
  said: '',
  template: '',
  intent: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
  exceptions: {},
  actions: [{ type: 'move', folder: 'Food' }],
  priority: id,
  stack: false,
  model: '',
  min_confidence: null,
  enabled: true,
  version: 1,
  created_at: 1791276732,
  updated_at: 1791276732,
  hits_week: 0,
  mailbox_removed: false,
  last_match_at: null,
  ...over,
});

const refusal = (code: string, message: string, path?: string) => ({ error: { code, message, path } });

/** Answers each "METHOD /path" with its [status, body]; anything else is a 404. */
function serve(routes: Record<string, [number, unknown?]>) {
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[`${init.method} ${url}`] ?? [404, refusal('not_found', 'No such route in this test.')];
    return new Response(body === undefined ? null : JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const ids = () => rules.list.map((r) => r.id);
const sent = (f: ReturnType<typeof serve>, call = 0) => JSON.parse(f.mock.calls[call][1].body as string);

beforeEach(() => {
  Object.assign(rules, { list: [rule(1), rule(2), rule(3)], loaded: true, error: '' });
  toast.text = '';
});
afterEach(() => vi.unstubAllGlobals());

describe('load', () => {
  it('takes the list in the order the daemon gives it', async () => {
    Object.assign(rules, { list: [], loaded: false });
    serve({ 'GET /api/rules': [200, { items: [rule(2, { hits_week: 38, last_match_at: 1791270000 }), rule(1)] }] });
    await load();
    expect(ids()).toEqual([2, 1]);
    expect(rules).toMatchObject({ loaded: true, error: '' });
    expect(rules.list[0]).toMatchObject({ hits_week: 38, last_match_at: 1791270000 });
  });

  it('knows an empty install from a failed load', async () => {
    Object.assign(rules, { list: [], loaded: false });
    serve({ 'GET /api/rules': [500, refusal('internal', 'Something went wrong on the server.')] });
    await load();
    expect(rules).toMatchObject({ list: [], loaded: false, error: 'Something went wrong on the server.' });

    serve({ 'GET /api/rules': [200, { items: [] }] });
    await load();
    expect(rules).toMatchObject({ list: [], loaded: true, error: '' });
  });
});

describe('move', () => {
  it('sends every id in the new order and keeps the daemon\'s answer', async () => {
    const f = serve({ 'POST /api/rules/reorder': [200, { items: [rule(2, { priority: 1 }), rule(1, { priority: 2 }), rule(3)] }] });
    await move(2, 0);
    expect(sent(f)).toEqual({ ids: [2, 1, 3] });
    expect(rules.list.map((r) => [r.id, r.priority])).toEqual([[2, 1], [1, 2], [3, 3]]);
  });

  it.each<[string, number, unknown, string]>([
    ['the daemon refuses the order', 400, refusal('invalid_input', "List every rule's id exactly once, in the new order.", 'ids'), "List every rule's id exactly once, in the new order."],
    ['the daemon is down', 502, undefined, ''],
  ])('puts the old order back when %s', async (_, status, body, flashed) => {
    serve({ 'POST /api/rules/reorder': [status, body] });
    const moving = move(3, 0);
    expect(ids()).toEqual([3, 1, 2]);
    await moving;
    expect(ids()).toEqual([1, 2, 3]);
    expect(toast.text).toBe(flashed);
  });

  it('asks nothing when the rule would not move', async () => {
    const f = serve({});
    await move(1, 0);
    await move(99, 1);
    expect(f).not.toHaveBeenCalled();
  });
});

describe('edit', () => {
  it('patches one rule and takes the saved rule from the envelope', async () => {
    const f = serve({ 'PATCH /api/rules/2': [200, { rule: rule(2, { enabled: false, version: 2 }) }] });
    await edit(2, { enabled: false });
    expect(sent(f)).toEqual({ enabled: false });
    expect(rules.list[1]).toMatchObject({ id: 2, enabled: false, version: 2 });
    expect(ids()).toEqual([1, 2, 3]);
  });

  it('hands a refused field to the caller and changes nothing', async () => {
    const message = 'min_confidence: a rule that trashes on intent needs min_confidence of at least 0.85';
    serve({ 'PATCH /api/rules/2': [400, refusal('rule_invalid', message, 'min_confidence')] });
    await expect(edit(2, { min_confidence: 0.6 })).rejects.toMatchObject({ status: 400, path: 'min_confidence', message });
    expect(rules.list[1].min_confidence).toBeNull();
  });
});

it('deletes a rule', async () => {
  const f = serve({ 'DELETE /api/rules/2': [204] });
  await remove(2);
  expect(f).toHaveBeenCalledOnce();
  expect(ids()).toEqual([1, 3]);
});

it('keeps a rule the daemon would not delete', async () => {
  serve({});
  await expect(remove(2)).rejects.toMatchObject({ status: 404 });
  expect(ids()).toEqual([1, 2, 3]);
});

describe('import', () => {
  it('takes every rule after the import from the result, with its counts', async () => {
    serve({ 'POST /api/rules/import': [200, { items: [rule(1), rule(2), rule(3), rule(4)], created: 1, updated: 2 }] });
    expect(await importFile(new Blob(['rules: []']))).toMatchObject({ created: 1, updated: 2 });
    expect(ids()).toEqual([1, 2, 3, 4]);
  });

  it('leaves the list alone when the file is refused', async () => {
    serve({ 'POST /api/rules/import': [400, refusal('rule_invalid', 'rule 3 ("Scams"): min_confidence: too low', 'min_confidence')] });
    await expect(importFile(new Blob(['x']))).rejects.toMatchObject({ message: 'rule 3 ("Scams"): min_confidence: too low' });
    expect(ids()).toEqual([1, 2, 3]);
  });
});

it('saves new rules through the batch route, surer for one that trashes', async () => {
  const f = serve({ 'POST /api/rules/batch': [201, { items: [rule(4), rule(5)] }] });
  const input = { name: 'New', said: 's', intent: 'spam', actions: [{ type: 'archive' as const }], stack: false, enabled: true };
  const added = await add([input, { ...input, actions: [{ type: 'trash' }] }]);
  expect(sent(f)).toEqual({ rules: [input, { ...input, actions: [{ type: 'trash' }], min_confidence: 0.9 }] });
  expect(added.map((r) => r.id)).toEqual([4, 5]);
  expect(ids()).toEqual([1, 2, 3, 4, 5]);
});

it('undoes what a rule did since local midnight', async () => {
  vi.useFakeTimers({ now: new Date(2026, 9, 6, 14, 30) });
  const f = serve({});
  await undoToday(2).catch(() => {});
  vi.useRealTimers();
  expect(f.mock.calls[0][0]).toBe('/api/rules/2/undo?since=' + new Date(2026, 9, 6).getTime() / 1000);
});
