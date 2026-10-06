import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Draft } from '../lib/api/compose';
import { compose } from '../lib/state/compose.svelte';
import { rules } from '../lib/state/rules.svelte';
import { toast } from '../lib/state/toast.svelte';
import Compose from './Compose.svelte';

/** Answers each "METHOD /path" with its [status, body]; anything else is a 404. */
function serve(routes: Record<string, [number, unknown]>) {
  const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[`${init.method} ${url}`] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

const typed = () => screen.getByLabelText<HTMLTextAreaElement>('What should happen to your email?');

async function describe(text: string) {
  render(Compose);
  await fireEvent.input(typed(), { target: { value: text } });
  await fireEvent.click(screen.getByRole('button', { name: 'Turn into rules' }));
}

beforeEach(() => {
  Object.assign(compose, { text: '', drafts: [], unparsed: [], busy: false, templates: [], needsModel: '' });
  Object.assign(rules, { list: [], loaded: true, error: '' });
  toast.text = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('shows why a draft is wrong and will not save it', async () => {
  // A draft as POST /api/rules/compose returns it when it fails validation.
  const draft: Draft = {
    name: 'Cold sales',
    said: 'Trash cold sales pitches',
    intent: 'Cold sales pitches',
    account_id: null,
    stack: false,
    model: '',
    conditions: {},
    exceptions: {},
    actions: [{ type: 'trash' }],
    min_confidence: 0.5,
    new_folders: [],
    question: null,
    conflicts: [],
    errors: [{ path: 'min_confidence', message: 'min_confidence: a rule that trashes on intent needs min_confidence of at least 0.85' }],
    match_count: 0,
    samples: [],
  };
  const f = serve({ 'POST /api/rules/compose': [200, { rules: [draft], unparsed: [] }] });
  await describe('Trash cold sales pitches');

  expect((await screen.findByRole('alert')).textContent).toBe(draft.errors[0].message);
  expect(screen.getByText('Cannot be saved as it is')).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Skip' })).toBeNull();
  // The count would read as "matches nothing" when it was never tested.
  expect(screen.queryByText(/of your last 200 emails/)).toBeNull();

  await fireEvent.click(screen.getByRole('button', { name: 'Save 0 rules' }));
  expect(f).toHaveBeenCalledOnce();
  expect(toast.text).toBe('Nothing to save: every draft is skipped');
});

it('says a model is needed, links to Settings and keeps what was typed', async () => {
  const message = 'This needs an AI model, and none is set up yet. Add a Claude (Anthropic) key in Settings, then try again. Rules built from conditions work without one.';
  serve({ 'POST /api/rules/compose': [409, { error: { code: 'no_composer_model', message } }] });
  await describe('Archive LinkedIn');

  expect((await screen.findByRole('alert')).textContent).toContain(message);
  expect(screen.getByRole('link', { name: 'Open Settings' }).getAttribute('href')).toBe('#/settings');
  expect(typed().value).toBe('Archive LinkedIn');
  expect(toast.text).toBe('');
});

it('puts a refused builder save on the row its path names, until that row is edited', async () => {
  const message = 'rules[0].conditions.all[1].value: pattern does not compile: error parsing regexp: missing closing ): `(?i)((`';
  const f = serve({ 'POST /api/rules/batch': [400, { error: { code: 'rule_invalid', message, path: 'rules[0].conditions.all[1].value' } }] });
  render(Compose);
  await fireEvent.click(screen.getByRole('button', { name: 'Build with conditions' }));
  // Three rows; the empty middle one is not sent, so the daemon's second condition is the third row.
  await fireEvent.click(screen.getByRole('button', { name: 'Add condition' }));
  await fireEvent.click(screen.getByRole('button', { name: 'Add condition' }));
  const values = screen.getAllByLabelText<HTMLInputElement>('Value');
  await fireEvent.input(values[0], { target: { value: 'acme.com' } });
  await fireEvent.change(screen.getAllByLabelText('Operator')[2], { target: { value: 'matches' } });
  await fireEvent.input(values[2], { target: { value: '((' } });
  await fireEvent.change(screen.getByLabelText('Action'), { target: { value: 'archive' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save rule' }));

  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toBe(message);
  expect(JSON.parse(f.mock.calls[0][1].body as string).rules[0].conditions.all).toHaveLength(2);
  expect(values.map((v) => v.getAttribute('aria-invalid'))).toEqual([null, null, 'true']);
  expect(values[2].getAttribute('aria-describedby')).toBe(alert.id);
  expect(values[2].parentElement?.contains(alert)).toBe(true);
  expect(toast.text).toBe('');

  await fireEvent.input(values[2], { target: { value: '(a|b)' } });
  expect(screen.queryByRole('alert')).toBeNull();
  expect(values[2].getAttribute('aria-invalid')).toBeNull();
});

it('flashes a refused builder save the form has no control for', async () => {
  const message = 'The model must be empty, or one of jev, clef, anthropic, openai, ollama, optionally followed by :model.';
  serve({ 'POST /api/rules/batch': [400, { error: { code: 'rule_invalid', message, path: 'rules[0].model' } }] });
  render(Compose);
  await fireEvent.click(screen.getByRole('button', { name: 'Build with conditions' }));
  await fireEvent.input(screen.getByLabelText('Value'), { target: { value: 'acme.com' } });
  await fireEvent.change(screen.getByLabelText('Action'), { target: { value: 'archive' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Save rule' }));

  await vi.waitFor(() => expect(toast.text).toBe(message));
  expect(screen.queryByRole('alert')).toBeNull();
});
