import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Settings, SettingsPatch } from '../api/settings';
import { load, patch, setKey, settings, toggleDryRun } from './settings.svelte';
import { toast } from './toast.svelte';

const SECRET = 'sk-ant-secret-123';

// GET /api/settings from a fresh daemon.
const fresh = (): Settings => ({
  openai_base_url: '',
  ollama_url: '',
  dry_run: true,
  decider: 'jev',
  decider_model: '',
  fallback_model: 'claude-haiku-4-5',
  composer_model: 'claude-haiku-4-5',
  escalate_below: 0.75,
  min_confidence: 0.75,
  retention_days: 30,
  keys: { openrouter_api_key: false, cloudflare_account_id: false, cloudflare_api_token: false, anthropic_api_key: false, openai_api_key: false },
  server: { version: 'dev', data_dir: './data', listen: '127.0.0.1:8080', mode: 'selfhost' },
  features: { digest: false, notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false },
});

let stored: Settings;
let refuse: { status: number; error?: object } | undefined;

/** A daemon that keeps settings: PATCH merges, and answers which keys are set, never the keys. */
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  if (url !== '/api/settings') throw new Error('unexpected ' + url);
  if (refuse) return new Response(refuse.error ? JSON.stringify({ error: refuse.error }) : null, { status: refuse.status });
  if (init.method === 'PATCH') {
    const { keys = {}, ...rest } = JSON.parse(init.body as string) as SettingsPatch;
    const set = Object.fromEntries(Object.entries(keys).map(([k, v]) => [k, v !== '']));
    stored = { ...stored, ...rest, keys: { ...stored.keys, ...set } };
  }
  return new Response(JSON.stringify(stored), { status: 200 });
});
const sent = () => JSON.parse(fetchMock.mock.calls.at(-1)![1].body as string);

beforeEach(async () => {
  stored = fresh();
  refuse = undefined;
  fetchMock.mockClear();
  vi.stubGlobal('fetch', fetchMock);
  await load();
});
afterEach(() => vi.unstubAllGlobals());

it('loads the settings in force', () => {
  expect(settings).toMatchObject({ value: fresh(), loaded: true, error: '' });
});

it('reports a failed load and recovers on retry', async () => {
  refuse = { status: 500, error: { code: 'internal', message: 'Something went wrong.' } };
  await load();
  expect(settings.error).toBe('Something went wrong.');
  refuse = undefined;
  await load();
  expect(settings.error).toBe('');
});

it.each<[string, SettingsPatch]>([
  ['decider with its model', { decider: 'ollama', decider_model: 'llama3.2' }],
  ['fallback off', { fallback_model: '' }],
  ['composer model', { composer_model: 'claude-sonnet-5' }],
  ['escalation threshold', { escalate_below: 0.6 }],
  ['act threshold', { min_confidence: 0.9 }],
  ['retention', { retention_days: 3650 }],
])('patches %s and keeps the rest', async (_name, change) => {
  expect(await patch(change)).toBe(true);
  expect(sent()).toEqual(change);
  expect(settings.value).toEqual({ ...fresh(), ...change });
});

it('shows why a change was refused and keeps the saved value', async () => {
  refuse = { status: 400, error: { code: 'invalid_input', message: 'ollama has no default model: name one', path: 'decider_model' } };
  expect(await patch({ decider: 'ollama' })).toBe(false);
  expect(toast.text).toBe('ollama has no default model: name one');
  expect(settings.value).toEqual(fresh());
});

it('toggles dry-run both ways with the matching toast', async () => {
  await toggleDryRun();
  expect(sent()).toEqual({ dry_run: false });
  expect(settings.value.dry_run).toBe(false);
  expect(toast.text).toBe('Live: MailRules is sorting your mail again');
  await toggleDryRun();
  expect(settings.value.dry_run).toBe(true);
  expect(toast.text).toBe('Dry-run on: nothing in your mailbox will change');
});

it('sends a key once in the body and keeps only that it is set', async () => {
  await setKey('anthropic_api_key', SECRET, 'Anthropic API key');

  expect(sent()).toEqual({ keys: { anthropic_api_key: SECRET } });
  expect(fetchMock.mock.calls.map((c) => c[0]).join()).not.toContain(SECRET);
  expect(settings.value.keys.anthropic_api_key).toBe(true);
  expect(toast.text).toBe('Anthropic API key saved');
  expect(JSON.stringify([settings, { ...localStorage }, { ...sessionStorage }])).not.toContain(SECRET);
});

it('removes a stored key with an empty string', async () => {
  await setKey('openai_api_key', SECRET, 'OpenAI API key');
  await setKey('openai_api_key', '', 'OpenAI API key');
  expect(sent()).toEqual({ keys: { openai_api_key: '' } });
  expect(settings.value.keys.openai_api_key).toBe(false);
  expect(toast.text).toBe('OpenAI API key removed');
});
