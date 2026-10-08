import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Settings as Saved } from '../lib/api/settings';
import { accounts } from '../lib/state/accounts.svelte';
import { firstRun, type Step } from '../lib/state/firstrun.svelte';
import { load, settings } from '../lib/state/settings.svelte';
import Setup from './Setup.svelte';

// GET /api/settings from a fresh daemon with no keys anywhere.
const fresh = (over: Partial<Saved> = {}): Saved => ({
  dry_run: true, decider: 'jev', decider_model: '', fallback_model: 'claude-haiku-4-5', composer_model: 'claude-haiku-4-5',
  escalate_below: 0.75, min_confidence: 0.75, retention_days: 30, trash_to_folder: true, leave_own_mail: true, openai_base_url: '', ollama_url: '',
  anthropic_workspace_id: '', anthropic_workspace_name: '', anthropic_workspace_found: false,
  keys: { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' },
  warnings: [],
  server: { version: 'dev', data_dir: './data', listen: '127.0.0.1:8080', mode: 'selfhost' },
  limits: { test_default: 200, test_max: 2000, check_max: 2000 },
  features: { digest: false, notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false, suggest: false },
  ...over,
});
const warn = (code: 'decider_not_ready' | 'composer_not_ready', path: string, message: string) => ({ code, path, message });
// The warnings a daemon with no keys answers.
const noOpenRouter = warn('decider_not_ready', 'keys.openrouter_api_key', 'The jev decision model needs an OpenRouter API key. Until it is set, rules that need a model are passed over.');
const noClaude = warn('composer_not_ready', 'keys.anthropic_api_key', 'The rule composer model, claude-haiku-4-5, needs a Claude (Anthropic) API key. Until it is set, Describe it, Rewrite with AI and Suggest from my mail do not work.');
const noCloudflareId = warn('decider_not_ready', 'keys.cloudflare_account_id', 'The clef decision model needs a Cloudflare account ID. Until it is set, rules that need a model are passed over.');
const noCloudflareToken = warn('decider_not_ready', 'keys.cloudflare_api_token', 'The clef decision model needs a Cloudflare API token. Until it is set, rules that need a model are passed over.');
const noOllama = warn('decider_not_ready', 'ollama_url', 'The ollama decision model needs the URL of your Ollama server. Until it is set, rules that need a model are passed over.');

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const r = routes[`${init.method} ${url}`];
  if (!r) throw new Error(`unexpected ${init.method} ${url}`);
  return new Response(JSON.stringify(r[1]), { status: r[0] });
});
const patches = () => fetchMock.mock.calls.filter((c) => c[1].method === 'PATCH').map((c) => JSON.parse(c[1].body as string));
const statuses = () => screen.queryAllByRole('status').map((s) => s.textContent);
const continueButton = () => screen.getByRole('button', { name: 'Continue' }) as HTMLButtonElement;
/** The provider key fields on the step, top to bottom, by their label. */
const keyFields = () => screen.queryAllByLabelText(/API key|Cloudflare/).map((f) => f.closest('label')!.firstElementChild!.firstChild!.textContent!.trim());

/** Opens the guide on `step` the way the shell does: the settings are loaded first. */
async function show(step: Step, saved = fresh({ warnings: [noOpenRouter, noClaude] })) {
  routes['GET /api/settings'] = [200, saved];
  await load();
  firstRun.step = step;
  render(Setup);
}

beforeEach(() => {
  routes = {
    'GET /api/presets': [200, { items: [{ name: 'icloud', label: 'iCloud Mail', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: true, secret_label: 'App-specific password' }] }],
  };
  fetchMock.mockClear();
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(settings, { loaded: false, error: '' });
  Object.assign(accounts, { list: [], loaded: true, error: '', presets: [], presetsError: '' });
  Object.assign(firstRun, { offered: false, step: null });
  location.hash = '#/setup';
});
afterEach(() => vi.unstubAllGlobals());

it('gives way to Overview when it was not opened by creating the admin account', async () => {
  routes['GET /api/settings'] = [200, fresh()];
  await load();
  render(Setup);
  expect(screen.queryByRole('heading', { name: 'Welcome to MailRules' })).toBeNull();
  await vi.waitFor(() => expect(location.hash).toBe('#/'));
});

it('shows the warnings of a fresh install beside the decision model, with the keys they name, and holds Continue', async () => {
  await show('ai');
  expect(screen.getByRole('listitem', { current: 'step' }).textContent).toContain('Decision model');
  expect((screen.getByLabelText('Decision model') as HTMLSelectElement).value).toBe('jev');
  expect(statuses()).toEqual([noOpenRouter.message, noClaude.message]);
  // The decider's key first, then the one the composer's warning names; no other provider.
  expect(keyFields()).toEqual(['OpenRouter API key', 'Anthropic API key']);
  expect(screen.getAllByText('Needed')).toHaveLength(2);
  expect(screen.queryByText('Other providers')).toBeNull();
  expect(continueButton().disabled).toBe(true);
});

it('saves the key the decider needs and lets the user continue to the mailbox', async () => {
  await show('ai');
  routes['PATCH /api/settings'] = [200, fresh({ keys: { ...fresh().keys, openrouter_api_key: 'stored' }, warnings: [noClaude] })];
  await fireEvent.input(screen.getByLabelText(/^OpenRouter API key/), { target: { value: ' sk-or-v1-abc ' } });
  await fireEvent.submit(screen.getByLabelText(/^OpenRouter API key/).closest('form')!);
  await waitFor(() => expect(statuses()).toEqual([noClaude.message]));
  expect(patches()).toEqual([{ keys: { openrouter_api_key: 'sk-or-v1-abc' } }]);
  expect((screen.getByLabelText(/^OpenRouter API key/) as HTMLInputElement).value).toBe('');

  await fireEvent.click(continueButton());
  expect(firstRun.step).toBe('mailbox');
  expect(await screen.findByRole('heading', { name: 'Where is your email?' })).toBeTruthy();
});

it.each<[name: string, pick: Saved['decider'], model: string, saved: Saved, fields: string[]]>([
  ['Clef asks for the Cloudflare account ID and token', 'clef', '',
    fresh({ decider: 'clef', warnings: [noCloudflareId, noCloudflareToken, noClaude] }), ['Cloudflare account ID', 'Cloudflare API token', 'Anthropic API key']],
  ['Claude asks for the Anthropic key alone', 'anthropic', '',
    fresh({ decider: 'anthropic', warnings: [warn('decider_not_ready', 'keys.anthropic_api_key', 'The anthropic decision model needs a Claude (Anthropic) API key. Until it is set, rules that need a model are passed over.')] }), ['Anthropic API key']],
  ['Ollama asks for its URL, once a model is named', 'ollama', 'llama3.2',
    fresh({ decider: 'ollama', decider_model: 'llama3.2', warnings: [noOllama, noClaude] }), ['Anthropic API key', 'Ollama server URL']],
])('saves the decider picked: %s, and shows its warnings', async (_name, pick, model, saved, fields) => {
  await show('ai');
  routes['PATCH /api/settings'] = [200, saved];
  await fireEvent.change(screen.getByLabelText('Decision model'), { target: { value: pick } });
  if (model) {
    await fireEvent.input(screen.getByLabelText('Decision model name'), { target: { value: model } });
    await fireEvent.change(screen.getByLabelText('Decision model name'));
  }
  await waitFor(() => expect(statuses()).toEqual(saved.warnings.map((w) => w.message)));
  expect(patches()).toEqual([{ decider: pick, decider_model: model }]);
  const shown = keyFields();
  if (screen.queryByLabelText('Ollama server URL')) shown.push('Ollama server URL');
  expect(shown).toEqual(fields);
  expect(continueButton().disabled).toBe(true);
});

it('takes an OpenAI-compatible endpoint: its URL shows as soon as it is picked, and its key with it', async () => {
  await show('ai');
  await fireEvent.change(screen.getByLabelText('Decision model'), { target: { value: 'openai' } });
  expect(screen.getByLabelText('Endpoint URL')).toBeTruthy();
  // Its key first, then the ones the saved settings' warnings name.
  expect(keyFields()).toEqual(['OpenAI API key', 'OpenRouter API key', 'Anthropic API key']);
  expect(patches()).toEqual([]); // no default model, so nothing is saved until one is named
});

it('skips the AI to the mailbox, and the mailbox to Overview', async () => {
  await show('ai');
  await fireEvent.click(screen.getByRole('button', { name: 'Skip for now' }));
  expect(firstRun.step).toBe('mailbox');
  expect(patches()).toEqual([]);

  await fireEvent.click(await screen.findByRole('button', { name: 'Skip for now' }));
  expect(firstRun.step).toBeNull();
  await vi.waitFor(() => expect(location.hash).toBe('#/'));
});

it('leaves for Overview from any step with Skip setup', async () => {
  await show('mailbox');
  await fireEvent.click(screen.getByRole('button', { name: 'Skip setup' }));
  expect(firstRun.step).toBeNull();
  await vi.waitFor(() => expect(location.hash).toBe('#/'));
});

it('shows the mail server refusing the sign-in on the mailbox step, and stays there', async () => {
  await show('mailbox');
  expect(screen.getByText(/Dry-run stays on/)).toBeTruthy();
  await fireEvent.click(await screen.findByRole('button', { name: /^iCloud Mail/ }));
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  routes['POST /api/accounts/test'] = [422, { error: { code: 'auth_failed', message: 'The mail server refused the sign-in.', path: 'password' } }];
  await fireEvent.input(screen.getByLabelText('iCloud Mail email address'), { target: { value: 'me@icloud.com' } });
  await fireEvent.input(screen.getByLabelText('App-specific password'), { target: { value: 'wrong-wrong-wrong' } });
  await fireEvent.click(screen.getByRole('button', { name: 'Test and continue' }));
  expect((await screen.findByRole('alert')).textContent).toBe('The mail server refused the sign-in.');
  expect(firstRun.step).toBe('mailbox');
});

it('ends on a closing word that opens Activity, with dry-run left on', async () => {
  await show('done');
  expect(screen.getByText(/watching your mailbox in dry-run/)).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Skip setup' })).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Open Activity' }));
  expect(firstRun.step).toBeNull();
  await vi.waitFor(() => expect(location.hash).toBe('#/activity'));
  expect(patches()).toEqual([]);
});
