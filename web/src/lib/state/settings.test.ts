import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Settings, SettingsPatch } from '../api/settings';
import { load, patch, setKey, settings, setUrl, toggleDryRun } from './settings.svelte';
import { toast } from './toast.svelte';

const SECRET = 'sk-ant-secret-123';

// GET /api/settings from a fresh daemon.
const fresh = (): Settings => ({
  openai_base_url: '',
  ollama_url: '',
  anthropic_workspace_id: '',
  anthropic_workspace_name: '',
  anthropic_workspace_found: false,
  dry_run: true,
  decider: 'jev',
  decider_model: '',
  fallback_model: 'claude-haiku-4-5',
  composer_model: 'claude-haiku-4-5',
  escalate_below: 0.75,
  min_confidence: 0.75,
  retention_days: 30,
  trash_to_folder: true,
  leave_own_mail: true,
  keys: { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' },
  warnings: [],
  server: { version: 'dev', data_dir: './data', listen: '127.0.0.1:8080', mode: 'selfhost' },
  limits: { test_default: 200, test_max: 2000, check_max: 2000 },
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
    const set = Object.fromEntries(Object.entries(keys ?? {}).map(([k, v]) => [k, v ? 'stored' : 'none']));
    // null puts the environment's default back, which this daemon has none of.
    const values = Object.fromEntries(Object.entries(rest).map(([k, v]) => [k, v ?? fresh()[k as keyof Settings]]));
    stored = { ...stored, ...values, keys: { ...stored.keys, ...set } };
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

it('keeps the newest load when an older one answers late', async () => {
  const answers: ((r: Response) => void)[] = [];
  fetchMock.mockImplementationOnce(() => new Promise((r) => answers.push(r)));
  fetchMock.mockImplementationOnce(() => new Promise((r) => answers.push(r)));
  const first = load();
  const second = load();
  answers[1](new Response(JSON.stringify({ ...fresh(), dry_run: false }), { status: 200 }));
  await second;
  answers[0](new Response(JSON.stringify({ error: { code: 'internal', message: 'Old failure.' } }), { status: 500 }));
  await first;
  expect(settings).toMatchObject({ value: { dry_run: false }, error: '' });
});

it('keeps a saved change over a load that was sent before it', async () => {
  let answer!: (r: Response) => void;
  fetchMock.mockImplementationOnce(() => new Promise((r) => (answer = r)));
  const slow = load();
  await patch({ dry_run: false });
  answer(new Response(JSON.stringify(fresh()), { status: 200 }));
  await slow;
  expect(settings.value.dry_run).toBe(false);
});

it.each<[string, SettingsPatch]>([
  ['decider with its model', { decider: 'ollama', decider_model: 'llama3.2' }],
  ['fallback off', { fallback_model: '' }],
  ['composer model', { composer_model: 'claude-sonnet-5' }],
  ['escalation threshold', { escalate_below: 0.6 }],
  ['act threshold', { min_confidence: 0.9 }],
  ['retention', { retention_days: 3650 }],
  ['trash to the server Trash', { trash_to_folder: false }],
  ['own mail sorted like any other', { leave_own_mail: false }],
])('patches %s and keeps the rest', async (_name, change) => {
  expect(await patch(change)).toBe(true);
  expect(sent()).toEqual(change);
  expect(settings.value).toEqual({ ...fresh(), ...change });
});

it('shows why a change was refused and keeps the saved value', async () => {
  refuse = { status: 400, error: { code: 'invalid_input', message: 'The ollama decider has no default model. Name one.', path: 'decider_model' } };
  expect(await patch({ decider: 'ollama' })).toBe(false);
  expect(toast.text).toBe('The ollama decider has no default model. Name one.');
  expect(settings.value).toEqual(fresh());
});

it('saves a decider URL, removes it with an empty string, and hands back a refusal of that field', async () => {
  expect(await setUrl('ollama_url', 'http://localhost:11434')).toBe('');
  expect(sent()).toEqual({ ollama_url: 'http://localhost:11434' });
  expect(settings.value.ollama_url).toBe('http://localhost:11434');

  expect(await setUrl('ollama_url', '')).toBe('');
  expect(sent()).toEqual({ ollama_url: null });
  expect(settings.value.ollama_url).toBe('');

  toast.text = '';
  refuse = { status: 400, error: { code: 'invalid_input', message: 'Enter an http or https URL, such as http://localhost:11434.', path: 'openai_base_url' } };
  expect(await setUrl('openai_base_url', 'not a url')).toBe('Enter an http or https URL, such as http://localhost:11434.');
  expect([toast.text, settings.value.openai_base_url]).toEqual(['', '']);

  refuse = { status: 500, error: { code: 'internal', message: 'Something went wrong.' } };
  expect(await setUrl('openai_base_url', 'https://llm.example.test/v1')).toBe('');
  expect(toast.text).toBe('Something went wrong.');
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
  expect(settings.value.keys.anthropic_api_key).toBe('stored');
  expect(toast.text).toBe('Anthropic API key saved');
  expect(JSON.stringify([settings, { ...localStorage }, { ...sessionStorage }])).not.toContain(SECRET);
});

it('removes a stored key with an empty string', async () => {
  await setKey('openai_api_key', SECRET, 'OpenAI API key');
  await setKey('openai_api_key', '', 'OpenAI API key');
  expect(sent()).toEqual({ keys: { openai_api_key: '' } });
  expect(settings.value.keys.openai_api_key).toBe('none');
  expect(toast.text).toBe('OpenAI API key removed');
});
