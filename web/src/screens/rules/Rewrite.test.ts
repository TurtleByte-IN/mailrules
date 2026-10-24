import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Draft } from '../../lib/api/compose';
import type { Rule } from '../../lib/api/rules';
import { accounts } from '../../lib/state/accounts.svelte';
import { rules } from '../../lib/state/rules.svelte';
import { settings } from '../../lib/state/settings.svelte';
import { toast } from '../../lib/state/toast.svelte';
import Rules from '../Rules.svelte';

// A saved rule as GET /api/rules returns it.
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
  mailbox_removed: false,
  notify: [],
  draft: null,
  last_match_at: null,
};
const other: Rule = { ...food, id: 8, name: 'Bills', priority: 2 };

// What POST /api/rules/7/compose answers for "also skip anything from my bank".
const draft: Draft = {
  name: 'Food',
  said: 'Swiggy goes to Food, but not my bank',
  intent: null,
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
  exceptions: { any: [{ field: 'from_domain', op: 'in', value: ['hdfcbank.com'] }] },
  actions: [{ type: 'move', folder: 'Food' }],
  account_id: null,
  stack: false,
  model: '',
  min_confidence: null,
  new_folders: [],
  question: null,
  conflicts: [],
  errors: [],
  match_count: 2,
  tested: 200,
  samples: [],
};

type Call = { method: string; url: string; body: unknown };
/** Answers each "METHOD /path" with [status, body]; every call is recorded. */
function serve(routes: Record<string, [number, unknown] | ((body: unknown) => [number, unknown])>) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const body = init.body ? JSON.parse(String(init.body)) : undefined;
      calls.push({ method: init.method!, url, body });
      const route = routes[`${init.method} ${url}`];
      const [status, answer] = (typeof route === 'function' ? route(body) : route) ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
      return new Response(JSON.stringify(answer), { status });
    }),
  );
  return calls;
}

const typed = () => screen.getByLabelText<HTMLTextAreaElement>('What should change about this rule?');

async function open(routes: Parameters<typeof serve>[0]) {
  const calls = serve(routes);
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Rewrite with AI' }));
  await fireEvent.input(typed(), { target: { value: 'also skip anything from my bank' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Rewrite rule' }));
  return calls;
}

beforeEach(() => {
  // The limits GET /api/settings reports.
  Object.assign(settings.value, { limits: { test_default: 200, test_max: 2000, check_max: 2000 } });
  Object.assign(rules, { list: [food, other], loaded: true, error: '' });
  Object.assign(accounts, { list: [], loaded: true });
  toast.text = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('offers Rewrite with AI on a saved rule, and the box stays closed until it is clicked', () => {
  render(Rules);
  expect(screen.getByRole('button', { name: 'Rewrite with AI' })).toBeTruthy();
  expect(screen.queryByLabelText('What should change about this rule?')).toBeNull();
});

it('sends the text for that rule and shows the one draft beside the current rule', async () => {
  const calls = await open({ 'POST /api/rules/7/compose': [200, { rule: draft, changes: [], matching: [] }] });

  const now = await screen.findByRole('region', { name: 'Current rule' });
  const next = screen.getByRole('region', { name: 'New draft' });
  expect(calls).toEqual([{ method: 'POST', url: '/api/rules/7/compose', body: { text: 'also skip anything from my bank' } }]);
  expect(within(now).queryByText('Unless')).toBeNull();
  expect(within(next).getByText('Unless')).toBeTruthy();
  expect(within(next).getByText(/hdfcbank\.com/)).toBeTruthy();
});

it('approving updates the same rule in place, with no create or delete', async () => {
  const calls = await open({
    'POST /api/rules/7/compose': [200, { rule: draft, changes: [], matching: [] }],
    'PATCH /api/rules/7': (body) => [200, { rule: { ...food, ...(body as object), version: 2 } }],
  });
  await fireEvent.click(await screen.findByRole('button', { name: 'Update rule' }));
  await vi.waitFor(() => expect(toast.text).toBe('Food updated'));

  expect(calls.map((c) => `${c.method} ${c.url}`)).toEqual(['POST /api/rules/7/compose', 'PATCH /api/rules/7']);
  expect(calls[1].body).toMatchObject({ name: 'Food', intent: '', exceptions: draft.exceptions, said: draft.said });
  expect(rules.list.map((r) => r.id)).toEqual([7, 8]);
  expect(rules.list[0].exceptions).toEqual(draft.exceptions);
  expect(screen.queryByLabelText('What should change about this rule?')).toBeNull();
});

it('discarding the draft saves nothing and leaves the rule as it was', async () => {
  const calls = await open({ 'POST /api/rules/7/compose': [200, { rule: draft, changes: [], matching: [] }] });
  await fireEvent.click(await screen.findByRole('button', { name: 'Discard' }));

  expect(screen.queryByRole('region', { name: 'New draft' })).toBeNull();
  expect(calls).toHaveLength(1);
  expect(rules.list[0]).toEqual(food);
});

it('says a model is needed and links to Settings', async () => {
  const message = 'This needs an AI model. The rule composer model, claude-haiku-4-5, needs a Claude (Anthropic) API key: add it in Settings, then try again. Rules built from conditions work without one.';
  await open({ 'POST /api/rules/7/compose': [409, { error: { code: 'no_composer_model', message } }] });

  expect((await screen.findByRole('alert')).textContent).toContain(message);
  expect(screen.getByRole('link', { name: 'Open Settings' }).getAttribute('href')).toBe('#/settings');
  expect(typed().value).toBe('also skip anything from my bank');
});

it('says the Claude workspace is needed and links to Settings', async () => {
  const message = 'Your Claude key covers your whole organisation, so MailRules needs to know which workspace to use. Choose it in Settings.';
  await open({ 'POST /api/rules/7/compose': [409, { error: { code: 'anthropic_workspace_needed', message } }] });

  expect((await screen.findByRole('alert')).textContent).toContain(message);
  expect(screen.getByRole('link', { name: 'Open Settings' }).getAttribute('href')).toBe('#/settings');
  expect(toast.text).toBe('');
});

it("shows the daemon's message when the model fails", async () => {
  await open({ 'POST /api/rules/7/compose': [502, { error: { code: 'model_error', message: 'The model could not be reached. Try again.' } }] });
  await vi.waitFor(() => expect(toast.text).toBe('The model could not be reached. Try again.'));
  expect(screen.queryByRole('region', { name: 'New draft' })).toBeNull();
});

it('will not update when the draft is the same as the rule or has errors', async () => {
  await open({ 'POST /api/rules/7/compose': [200, { rule: { ...draft, said: food.said, exceptions: {} }, changes: [], matching: [] }] });
  expect((await screen.findByText('This is the same as the current rule.')).getAttribute('role')).toBe('status');
  expect((screen.getByRole('button', { name: 'Update rule' }) as HTMLButtonElement).disabled).toBe(true);

  cleanup();
  await open({ 'POST /api/rules/7/compose': [200, { rule: { ...draft, errors: [{ path: 'actions', message: 'A rule needs an action.' }] }, changes: [], matching: [] }] });
  expect((await screen.findByRole('alert')).textContent).toBe('A rule needs an action.');
  expect((screen.getByRole('button', { name: 'Update rule' }) as HTMLButtonElement).disabled).toBe(true);
});

it('shows a refused update and keeps the draft', async () => {
  await open({
    'POST /api/rules/7/compose': [200, { rule: draft, changes: [], matching: [] }],
    'PATCH /api/rules/7': [400, { error: { code: 'rule_invalid', message: 'That name is taken.', path: 'name' } }],
  });
  await fireEvent.click(await screen.findByRole('button', { name: 'Update rule' }));
  expect((await screen.findByText('That name is taken.')).getAttribute('role')).toBe('alert');
  expect(screen.getByRole('region', { name: 'New draft' })).toBeTruthy();
});

// What POST /api/rules/7/compose answers for Improve rule: a draft that adds an exception,
// also suggests marking them read and a threshold, says why, and the emails that shift.
const improved = {
  rule: { ...draft, said: food.said, actions: [{ type: 'move', folder: 'Food' }, { type: 'read' }], min_confidence: 0.9 },
  changes: ['Skips your bank: you moved 2 of its emails out of Food.'],
  matching: [
    {
      change: 'stops',
      before: { from: 'alerts@hdfcbank.com', subject: 'Statement ready', received_at: null, stage: 'condition', rule_id: 7, rule_name: 'Food', confidence: 1, reason: 'Matched "Food" by its conditions', review: false, actions: [] },
      after: { from: 'alerts@hdfcbank.com', subject: 'Statement ready', received_at: null, stage: 'none', rule_id: null, rule_name: '', confidence: 0, reason: '', review: false, actions: [] },
    },
    {
      change: 'starts',
      before: { from: 'orders@zomato.com', subject: 'Your order', received_at: null, stage: 'none', rule_id: null, rule_name: '', confidence: 0, reason: '', review: false, actions: [] },
      after: { from: 'orders@zomato.com', subject: 'Your order', received_at: null, stage: 'decider', rule_id: null, rule_name: 'Food', confidence: 0.93, reason: 'A food delivery order', review: false, actions: [] },
    },
  ],
};

async function improve(routes: Parameters<typeof serve>[0]) {
  const calls = serve(routes);
  render(Rules);
  await fireEvent.click(screen.getByRole('button', { name: 'Improve rule' }));
  return calls;
}

it('Improve rule asks with no text and shows the rules side by side, what changed and the emails that shift', async () => {
  const calls = await improve({ 'POST /api/rules/7/compose': [200, improved] });

  const next = await screen.findByRole('region', { name: 'New draft' });
  expect(calls).toEqual([{ method: 'POST', url: '/api/rules/7/compose', body: { text: '' } }]);
  expect(typed().value).toBe('');
  expect(within(screen.getByRole('region', { name: 'Current rule' })).queryByText('Unless')).toBeNull();
  expect(within(next).getByText('Unless')).toBeTruthy();
  expect(within(screen.getByRole('region', { name: 'What changed' })).getByText(improved.changes[0])).toBeTruthy();
  const starts = screen.getByRole('region', { name: 'Starts matching' });
  expect(within(starts).getByText('Your order')).toBeTruthy();
  expect(within(starts).getByText('A food delivery order (0.93)')).toBeTruthy();
  const stops = screen.getByRole('region', { name: 'Stops matching' });
  expect(within(stops).getByText('Statement ready')).toBeTruthy();
  expect(within(stops).getByText('Matched "Food" by its conditions (1.00)')).toBeTruthy();
});

it('a new action or threshold is shown apart, unticked, and not saved unless ticked', async () => {
  const calls = await improve({
    'POST /api/rules/7/compose': [200, improved],
    'PATCH /api/rules/7': (body) => [200, { rule: { ...food, ...(body as object), version: 2 } }],
  });
  const also = await screen.findByRole('region', { name: 'Also suggested' });
  const boxes = within(also).getAllByRole<HTMLInputElement>('checkbox');
  expect(boxes.map((b) => b.checked)).toEqual([false, false]);
  expect(within(also).getByText('Change what it does: Move to Food → Move to Food, mark read')).toBeTruthy();
  expect(within(also).getByText('Change act when sure above: your default → 0.90')).toBeTruthy();
  // The new draft column shows what Update rule saves: the rule's own actions for now.
  expect(within(screen.getByRole('region', { name: 'New draft' })).queryByText(/mark read/)).toBeNull();

  await fireEvent.click(screen.getByRole('button', { name: 'Update rule' }));
  await vi.waitFor(() => expect(toast.text).toBe('Food updated'));
  expect(calls[1].body).toMatchObject({ actions: food.actions, min_confidence: null, exceptions: draft.exceptions });
});

it('a ticked action change is saved, and the threshold stays unless it is ticked too', async () => {
  const calls = await improve({
    'POST /api/rules/7/compose': [200, improved],
    'PATCH /api/rules/7': (body) => [200, { rule: { ...food, ...(body as object), version: 2 } }],
  });
  const [actions] = within(await screen.findByRole('region', { name: 'Also suggested' })).getAllByRole<HTMLInputElement>('checkbox');
  await fireEvent.click(actions);
  expect(within(screen.getByRole('region', { name: 'New draft' })).getByText(/mark read/)).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Update rule' }));
  await vi.waitFor(() => expect(toast.text).toBe('Food updated'));
  expect(calls[1].body).toMatchObject({ actions: improved.rule.actions, min_confidence: null });
});

it('will not update when only an unticked action change differs', async () => {
  await improve({ 'POST /api/rules/7/compose': [200, { ...improved, rule: { ...improved.rule, exceptions: {}, min_confidence: null } }] });
  const update = await screen.findByRole<HTMLButtonElement>('button', { name: 'Update rule' });
  expect(update.disabled).toBe(true);
  await fireEvent.click(within(screen.getByRole('region', { name: 'Also suggested' })).getByRole('checkbox'));
  expect(update.disabled).toBe(false);
});
