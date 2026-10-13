import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Destination } from '../lib/api/notifications';
import type { Rule } from '../lib/api/rules';
import { auth } from '../lib/state/auth.svelte';
import { notifications } from '../lib/state/notifications.svelte';
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
  notify: [],
  draft: null,
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

describe('Notify', () => {
  const dest = (id: number, name: string): Destination => ({ id, name, kind: 'slack', url: 'https://hooks.slack.com/…bX9z', address_deleted: false, needs_https: false, created_at: 1791276732, deliveries: [] });
  const group = () => screen.queryByRole('group', { name: 'Notify' });

  beforeEach(() => {
    settings.value.features.notifications = true;
    Object.assign(notifications, { kinds: ['slack'], destinations: [dest(1, 'Team'), dest(2, 'Ops')], loaded: true, error: '' });
  });
  afterEach(() => {
    settings.value.features.notifications = false;
    Object.assign(notifications, { kinds: [], destinations: [], loaded: false, error: '' });
  });

  it('shows no Notify group in a build without the notifications module', () => {
    settings.value.features.notifications = false;
    Object.assign(rules, { list: [{ ...rule(1, 'Food'), notify: [1] }], loaded: true, error: '' });
    render(Rules);
    expect(group()).toBeNull();
  });

  it('ticks the destinations a rule tells, names them in the list, and saves a ticked one', async () => {
    Object.assign(rules, { list: [{ ...rule(1, 'Food'), notify: [2] }], loaded: true, error: '' });
    render(Rules);
    expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).toContain('Then: Move to Food · notifies Ops · 3 this week');
    const boxes = within(group()!).getAllByRole('checkbox') as HTMLInputElement[];
    expect(boxes.map((b) => [b.parentElement!.textContent, b.checked])).toEqual([['Team', false], ['Ops', true]]);

    const f = respond(200, { rule: { ...rule(1, 'Food'), notify: [1, 2] } });
    await fireEvent.click(within(group()!).getByRole('checkbox', { name: 'Team' }));
    expect(f.mock.calls[0][0]).toBe('/api/rules/1');
    expect(f.mock.calls[0][1].method).toBe('PATCH');
    expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ notify: [1, 2] });
    await vi.waitFor(() => expect(rules.list[0].notify).toEqual([1, 2]));
    expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).toContain('notifies Team, Ops');
  });

  it('shows a refused notify under the Notify group', async () => {
    Object.assign(rules, { list: [rule(1, 'Food')], loaded: true, error: '' });
    render(Rules);
    const message = 'Destination 1 is not one of yours.';
    respond(400, { error: { code: 'invalid_input', message, path: 'notify' } });
    const box = within(group()!).getByRole('checkbox', { name: 'Team' });
    await fireEvent.click(box);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(message);
    expect(box.getAttribute('aria-describedby')).toBe(alert.id);
  });

  it('with no destinations, says where to add one', () => {
    notifications.destinations = [];
    Object.assign(rules, { list: [rule(1, 'Food')], loaded: true, error: '' });
    render(Rules);
    expect(group()!.textContent).toContain('No destinations yet.');
    expect(within(group()!).getByRole('link', { name: 'Settings' }).getAttribute('href')).toBe('#/settings');
  });
});

// A Member sorts by the team's rules but the daemon refuses their changes (403 role_required).
it('shows a Member the rules without a way to add, reorder, edit, test or delete them', async () => {
  respond(200, { items: [rule(1, 'Food'), rule(2, 'Travel')] });
  await load();
  auth.user = { id: 3, email: 'cy@example.com', role: 'member' };
  try {
    render(Rules);
    expect(screen.getByText('Shared with your team: only an Owner or Admin changes these.')).toBeTruthy();
    for (const name of ['Add rules', 'New condition rule', 'Edit conditions']) expect(screen.queryByRole('link', { name })).toBeNull();
    for (const name of ['Import rules', 'Move Food down', 'Rewrite with AI', 'Delete rule', 'Test on last 200 emails']) expect(screen.queryByRole('button', { name })).toBeNull();
    expect(screen.getByRole('button', { name: 'Export rules' })).toBeTruthy();
    // Disabled by the fieldset around the editor, which the element's own property does not show.
    expect(screen.getByLabelText('Name').matches(':disabled')).toBe(true);
    expect(screen.getAllByRole('checkbox', { name: 'On' }).every((b) => (b as HTMLInputElement).disabled)).toBe(true);
  } finally {
    auth.user = null;
  }
});

describe('Draft a reply', () => {
  const box = () => screen.queryByRole('checkbox', { name: 'Draft a reply (never sent automatically)' }) as HTMLInputElement | null;

  beforeEach(() => {
    settings.value.features.draft_replies = true;
  });
  afterEach(() => {
    settings.value.features.draft_replies = false;
  });

  it('is not offered in a build without the drafting module', () => {
    settings.value.features.draft_replies = false;
    Object.assign(rules, { list: [{ ...rule(1, 'Food'), draft: { instruction: 'polite no', read_previous: false } }], loaded: true, error: '' });
    render(Rules);
    expect(box()).toBeNull();
    expect(screen.queryByLabelText('How should the reply sound?')).toBeNull();
  });

  it('turns drafting on with an empty instruction, and says so in the list', async () => {
    Object.assign(rules, { list: [rule(1, 'Food')], loaded: true, error: '' });
    render(Rules);
    expect(box()!.checked).toBe(false);
    expect(screen.queryByLabelText('How should the reply sound?')).toBeNull();
    const on = { instruction: '', read_previous: false };
    const f = respond(200, { rule: { ...rule(1, 'Food'), draft: on } });
    await fireEvent.click(box()!);
    expect(f.mock.calls[0][1].method).toBe('PATCH');
    expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ draft: on });
    await vi.waitFor(() => expect(rules.list[0].draft).toEqual(on));
    expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).toContain('Then: Move to Food · draft a reply · 3 this week');
  });

  it('saves the instruction and the read-previous box, and turns drafting off', async () => {
    const d = { instruction: 'polite no', read_previous: false };
    Object.assign(rules, { list: [{ ...rule(1, 'Food'), draft: d }], loaded: true, error: '' });
    render(Rules);
    const how = screen.getByLabelText('How should the reply sound?') as HTMLInputElement;
    expect(how.value).toBe('polite no');
    expect(how.getAttribute('placeholder')).toBe('For example: polite no, not looking right now');

    let f = respond(200, { rule: { ...rule(1, 'Food'), draft: { ...d, instruction: 'warm yes' } } });
    await fireEvent.change(how, { target: { value: 'warm yes' } });
    expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ draft: { instruction: 'warm yes', read_previous: false } });
    await vi.waitFor(() => expect(rules.list[0].draft?.instruction).toBe('warm yes'));

    f = respond(200, { rule: { ...rule(1, 'Food'), draft: { instruction: 'warm yes', read_previous: true } } });
    await fireEvent.click(screen.getByRole('checkbox', { name: 'Also read the previous message in the thread' }));
    expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ draft: { instruction: 'warm yes', read_previous: true } });
    await vi.waitFor(() => expect(rules.list[0].draft?.read_previous).toBe(true));

    f = respond(200, { rule: rule(1, 'Food') });
    await fireEvent.click(box()!);
    expect(JSON.parse(String(f.mock.calls[0][1].body))).toEqual({ draft: null });
    await vi.waitFor(() => expect(screen.queryByLabelText('How should the reply sound?')).toBeNull());
  });

  it('shows a refused draft beside it', async () => {
    Object.assign(rules, { list: [rule(1, 'Food')], loaded: true, error: '' });
    render(Rules);
    const message = 'This build cannot draft replies.';
    respond(400, { error: { code: 'invalid_input', message, path: 'draft' } });
    await fireEvent.click(box()!);
    const alert = await screen.findByRole('alert');
    expect(alert.textContent).toBe(message);
    expect(box()!.getAttribute('aria-describedby')).toBe(alert.id);
  });
});
