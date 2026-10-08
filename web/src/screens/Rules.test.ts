import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Rule } from '../lib/api/rules';
import { load, rules } from '../lib/state/rules.svelte';
import { settings } from '../lib/state/settings.svelte';
import Rules from './Rules.svelte';

// A rule as GET /api/rules returns it.
const rule = (id: number, name: string): Rule => ({
  id,
  account_id: null,
  name,
  said: '',
  template: '',
  intent: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
  exceptions: {},
  actions: [{ type: 'move', folder: name }],
  priority: id,
  stack: false,
  model: '',
  min_confidence: null,
  enabled: true,
  version: 1,
  created_at: 1791276732,
  updated_at: 1791276732,
  hits_week: 3,
  mailbox_removed: false,
  last_match_at: null,
});

/** Answers every request with [status, body]. */
function respond(status: number, body: unknown) {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => new Response(JSON.stringify(body), { status }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

beforeEach(() => {
  Object.assign(rules, { list: [], loaded: false, error: '' });
  // The limits GET /api/settings reports.
  Object.assign(settings.value, { limits: { test_default: 200, test_max: 2000, check_max: 2000 } });
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('shows a failed load as an alert, and Retry fetches the rules again', async () => {
  respond(500, { error: { code: 'internal', message: 'Something went wrong on the server.' } });
  await load();
  render(Rules);
  expect(screen.getByRole('alert').textContent).toContain('Your rules could not be loaded. Something went wrong on the server.');
  expect(screen.queryByText('No rules yet.')).toBeNull();

  const f = respond(200, { items: [rule(1, 'Food')] });
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(f.mock.calls[0][0]).toBe('/api/rules');
  expect(await screen.findByRole('button', { name: /^Food/ })).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('says so when there are no rules', async () => {
  respond(200, { items: [] });
  await load();
  render(Rules);
  expect(screen.getByText('No rules yet.')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it.each([
  [true, 'Then: Move to MailRules Trash'],
  [false, 'Then: Move to Trash'],
])('with trash_to_folder %s, a trash rule names where its mail goes: %s', async (on, text) => {
  settings.value.trash_to_folder = on;
  try {
    respond(200, { items: [{ ...rule(1, 'Scams'), actions: [{ type: 'trash' }] }] });
    await load();
    render(Rules);
    expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).toContain(text);
  } finally {
    settings.value.trash_to_folder = true;
  }
});

it('lists the rules in order, and asks for a mailbox before testing one', async () => {
  const f = respond(200, { items: [rule(1, 'Food'), rule(2, 'Travel')] });
  await load();
  render(Rules);
  const rows = screen.getByRole('region', { name: 'Rule list' }).querySelectorAll('li');
  expect([...rows].map((li) => li.querySelector('.font-semibold')?.textContent)).toEqual(['Food', 'Travel']);
  expect(rows[0].textContent).toContain('from_domain in swiggy.in');
  expect(rows[0].textContent).toContain('Then: Move to Food · 3 this week');

  // No mailbox is connected: the tester sends nothing and says what to do.
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  expect(screen.getByRole('link', { name: 'Connect a mailbox' }).getAttribute('href')).toBe('#/accounts');
  expect(f).toHaveBeenCalledOnce();
});

it('a rule added from a template says so instead of quoting words the user never said', async () => {
  respond(200, { items: [{ ...rule(1, 'Receipts'), template: 'Receipts' }, { ...rule(2, 'Food'), said: 'Swiggy goes to Food' }] });
  await load();
  render(Rules);
  const editor = screen.getByRole('complementary', { name: 'Edit rule' });
  expect(editor.textContent).toContain('Added from the Receipts template');
  expect(editor.textContent).not.toContain('You said');

  await fireEvent.click(screen.getByRole('button', { name: /^Food/ }));
  expect(editor.textContent).toContain('You said');
  expect(editor.textContent).not.toContain('template');
});

it('states how rules are checked, and links the guide', async () => {
  respond(200, { items: [] });
  await load();
  render(Rules);
  expect(screen.getByText(/A rule with only conditions ends the check when it matches/)).toBeTruthy();
  expect(screen.queryByText(/first confident match wins/)).toBeNull();
  const link = screen.getByRole('link', { name: 'How rules are checked' });
  expect(link.getAttribute('href')).toBe('https://github.com/TurtleByte-IN/mailrules/blob/main/docs/guide/rules.md#how-an-email-is-decided');
});

const withModel = (model: string) => Object.assign(rules, { list: [{ ...rule(1, 'Food'), model }], loaded: true, error: '' });

it('shows a rule whose model is not in the list as that model, not as Default', () => {
  withModel('ollama:llama3.2');
  render(Rules);
  expect((screen.getByLabelText('Model') as HTMLSelectElement).selectedOptions[0].textContent).toBe('Ollama…');
  expect((screen.getByLabelText('Model as name:model') as HTMLInputElement).value).toBe('ollama:llama3.2');
});

it('saves openai:<model> from the editor, and shows the daemon refusal beside the field', async () => {
  withModel('');
  const f = respond(400, { error: { code: 'invalid_input', message: 'The ollama decider has no default model. Write it as ollama:<model>.', path: 'model' } });
  render(Rules);
  await fireEvent.change(screen.getByLabelText('Model'), { target: { value: 'ollama' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Set model' }));
  expect(f).toHaveBeenCalledOnce();
  expect(f.mock.calls[0][0]).toBe('/api/rules/1');
  expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ model: 'ollama:' });
  expect((await screen.findByRole('alert')).textContent).toContain('Write it as ollama:<model>.');
  expect(rules.list[0].model).toBe('');
});

it('saves openai:<model> typed in the editor and keeps showing it', async () => {
  withModel('');
  const f = respond(200, { rule: { ...rule(1, 'Food'), model: 'openai:gpt-4o-mini' } });
  render(Rules);
  await fireEvent.change(screen.getByLabelText('Model'), { target: { value: 'openai' } });
  await fireEvent.input(screen.getByLabelText('Model as name:model'), { target: { value: 'openai:gpt-4o-mini' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Set model' }));
  await vi.waitFor(() => expect(rules.list[0].model).toBe('openai:gpt-4o-mini'));
  expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ model: 'openai:gpt-4o-mini' });
  expect((screen.getByLabelText('Model') as HTMLSelectElement).selectedOptions[0].textContent).toBe('OpenAI-compatible…');
  expect((screen.getByLabelText('Model as name:model') as HTMLInputElement).value).toBe('openai:gpt-4o-mini');
});

it('marks a rule whose mailbox was removed and keeps its switch off until All mailboxes or a mailbox is chosen', async () => {
  const why = 'Its mailbox was removed. Choose a mailbox for it, or All mailboxes, or edit its condition, before turning it on.';
  Object.assign(rules, { list: [{ ...rule(1, 'Work'), enabled: false, mailbox_removed: true }, rule(2, 'Food')], loaded: true, error: '' });
  render(Rules);
  const rows = screen.getByRole('region', { name: 'Rule list' }).querySelectorAll('li');
  expect(rows[0].textContent).toContain('Its mailbox was removed');
  expect(rows[1].textContent).not.toContain('Its mailbox was removed');
  const [off, on] = screen.getAllByRole('checkbox', { name: 'On' }) as HTMLInputElement[];
  expect([off.disabled, on.disabled]).toEqual([true, false]);
  const describedBy = (off.getAttribute('aria-describedby') ?? '').split(' ').map((id) => document.getElementById(id)?.textContent);
  expect(describedBy).toEqual(['Its mailbox was removed', why]);
  expect(screen.getByRole('note').textContent).toBe(why);

  // The mailbox picker stays usable: All mailboxes is a choice too, and the daemon's answer clears the mark.
  const picker = screen.getByLabelText('Applies to') as HTMLSelectElement;
  expect(picker.selectedOptions[0].textContent).toBe('Choose a mailbox');
  const f = respond(200, { rule: { ...rule(1, 'Work'), enabled: false } });
  await fireEvent.change(picker, { target: { value: '' } });
  expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ account_id: null });
  await vi.waitFor(() => expect(rules.list[0].mailbox_removed).toBe(false));
  expect((screen.getAllByRole('checkbox', { name: 'On' })[0] as HTMLInputElement).disabled).toBe(false);
  expect(screen.queryByRole('note')).toBeNull();
  expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).not.toContain('Its mailbox was removed');
});
