import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Settings as Saved } from '../lib/api/settings';
import { load, settings } from '../lib/state/settings.svelte';
import Settings from './Settings.svelte';

// GET /api/settings from a fresh daemon.
const fresh = (over: Partial<Saved> = {}): Saved => ({
  dry_run: true, decider: 'jev', decider_model: '', fallback_model: 'claude-haiku-4-5', composer_model: 'claude-haiku-4-5',
  escalate_below: 0.75, min_confidence: 0.75, retention_days: 30, openai_base_url: '', ollama_url: '',
  keys: { openrouter_api_key: 'environment', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'stored', openai_api_key: 'none' },
  warnings: [],
  server: { version: 'dev', data_dir: './data', listen: '127.0.0.1:8080', mode: 'selfhost' },
  features: { digest: false, notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false },
  ...over,
});

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const r = routes[`${init.method} ${url}`];
  if (!r) throw new Error(`unexpected ${init.method} ${url}`);
  return new Response(JSON.stringify(r[1]), { status: r[0] });
});
const patches = () => fetchMock.mock.calls.filter((c) => c[1].method === 'PATCH').map((c) => JSON.parse(c[1].body as string));

/** Mounts the screen the way the shell does: the settings are loaded first. */
async function show(saved: Saved) {
  routes['GET /api/settings'] = [200, saved];
  await load();
  render(Settings);
}

beforeEach(() => {
  routes = {};
  fetchMock.mockClear();
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(settings, { loaded: false, error: '' });
});
afterEach(() => vi.unstubAllGlobals());

it('shows a failed load with Retry, and Retry fetches the settings again', async () => {
  routes['GET /api/settings'] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  await load();
  render(Settings);
  expect(screen.getByRole('alert').textContent).toContain('Could not load the settings. Something went wrong.');
  expect(screen.queryByLabelText('Decision model')).toBeNull();

  routes['GET /api/settings'] = [200, fresh()];
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  await screen.findByLabelText('Decision model');
  expect(screen.queryByRole('alert')).toBeNull();
});

it('renders the settings in force', async () => {
  await show(fresh());
  expect((screen.getByLabelText('Decision model') as HTMLSelectElement).value).toBe('jev');
  expect((screen.getByLabelText('Fallback model') as HTMLInputElement).value).toBe('claude-haiku-4-5');
  expect((screen.getByLabelText('Keep email snippets for') as HTMLInputElement).value).toBe('30');
  expect((screen.getByRole('checkbox') as HTMLInputElement).checked).toBe(true);
  // The chip says where each key comes from; only a stored key can be removed here.
  const chip = (label: RegExp) => screen.getByLabelText(label).closest('label')!.querySelector('.chip')!.textContent;
  expect([chip(/^Anthropic API key/), chip(/^OpenRouter API key/), chip(/^OpenAI API key/)]).toEqual(['Set', 'Set by environment', 'Not set']);
  expect(screen.getAllByRole('button', { name: /^Remove / }).map((b) => b.getAttribute('aria-label'))).toEqual(['Remove Anthropic API key']);
  expect(screen.getByText('Version dev · data in ./data · listening on 127.0.0.1:8080')).toBeTruthy();
});

it.each<[Saved['decider'], string | undefined, string | undefined]>([
  ['jev', undefined, undefined],
  ['openai', 'https://llm.example.test/v1', undefined],
  ['ollama', undefined, 'http://localhost:11434'],
])('shows the URL field of the %s decider only', async (decider, endpoint, ollama) => {
  await show(fresh({ decider, decider_model: 'm', openai_base_url: 'https://llm.example.test/v1', ollama_url: 'http://localhost:11434' }));
  const value = (label: string) => (screen.queryByLabelText(label) as HTMLInputElement | null)?.value;
  expect([value('Endpoint URL'), value('Ollama server URL')]).toEqual([endpoint, ollama]);
});

it('shows the URL field as soon as its decider is picked', async () => {
  await show(fresh());
  await fireEvent.change(screen.getByLabelText('Decision model'), { target: { value: 'ollama' } });
  expect(screen.getByLabelText('Ollama server URL')).toBeTruthy();
  expect(patches()).toEqual([]); // Ollama has no default model, so nothing is saved until one is named
});

it('saves a URL, shows a refusal beside the field, and removes the stored one with an empty value', async () => {
  await show(fresh({ decider: 'ollama', decider_model: 'llama3.2' }));
  const field = screen.getByLabelText('Ollama server URL') as HTMLInputElement;

  routes['PATCH /api/settings'] = [400, { error: { code: 'invalid_input', message: 'Enter an http or https URL, such as http://localhost:11434.', path: 'ollama_url' } }];
  await fireEvent.change(field, { target: { value: 'localhost:11434' } });
  expect((await screen.findByRole('alert')).textContent).toBe('Enter an http or https URL, such as http://localhost:11434.');
  expect(field.getAttribute('aria-invalid')).toBe('true');

  routes['PATCH /api/settings'] = [200, fresh({ decider: 'ollama', decider_model: 'llama3.2', ollama_url: 'http://localhost:11434' })];
  await fireEvent.change(field, { target: { value: ' http://localhost:11434 ' } });
  await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
  expect(settings.value.ollama_url).toBe('http://localhost:11434');

  routes['PATCH /api/settings'] = [200, fresh({ decider: 'ollama', decider_model: 'llama3.2' })];
  await fireEvent.change(field, { target: { value: '' } });
  await waitFor(() => expect(settings.value.ollama_url).toBe(''));
  expect(patches()).toEqual([{ ollama_url: 'localhost:11434' }, { ollama_url: 'http://localhost:11434' }, { ollama_url: null }]);
});
