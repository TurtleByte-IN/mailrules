import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Settings as Saved } from '../lib/api/settings';
import { load, settings } from '../lib/state/settings.svelte';
import Settings from './Settings.svelte';

// GET /api/settings from a fresh daemon.
const fresh = (over: Partial<Saved> = {}): Saved => ({
  dry_run: true, decider: 'jev', decider_model: '', fallback_model: 'claude-haiku-4-5', composer_model: 'claude-haiku-4-5',
  escalate_below: 0.75, min_confidence: 0.75, retention_days: 30, trash_to_folder: true, leave_own_mail: true, openai_base_url: '', ollama_url: '',
  anthropic_workspace_id: '', anthropic_workspace_name: '', anthropic_workspace_found: false,
  keys: { openrouter_api_key: 'environment', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'stored', openai_api_key: 'none' },
  warnings: [],
  server: { version: 'dev', data_dir: './data', listen: '127.0.0.1:8080', mode: 'selfhost' },
  limits: { test_default: 200, test_max: 2000, check_max: 2000 },
  features: { notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false, suggest: false },
  summary: { enabled: false, frequency: 'daily', weekday: 'monday', time: '08:00', time_zone: 'UTC', to: '', to_default: '', smtp: { configured: false, missing: [] }, last_sent_at: null, next_at: null },
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
  routes = { 'GET /api/settings/anthropic-workspaces': [200, { status: 'none_needed', workspaces: [] }] };
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
  expect((screen.getByRole('checkbox', { name: /^Dry-run/ }) as HTMLInputElement).checked).toBe(true);
  expect((screen.getByRole('checkbox', { name: /^Send trashed mail to MailRules Trash/ }) as HTMLInputElement).checked).toBe(true);
  expect((screen.getByRole('checkbox', { name: /^Leave my own emails alone/ }) as HTMLInputElement).checked).toBe(true);
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

it('says which forms the rule composer model takes', async () => {
  await show(fresh());
  const field = screen.getByLabelText('Rule composer model');
  const hint = document.getElementById(field.getAttribute('aria-describedby')!)!;
  expect(hint.textContent).toBe('A Claude model such as claude-haiku-4-5, or openai:gpt-4o-mini or ollama:llama3.2.');
});

it.each<[string, string | undefined, string | undefined]>([
  ['openai:gpt-4o-mini', 'https://llm.example.test/v1', undefined],
  ['ollama:llama3.2', undefined, 'http://localhost:11434'],
  ['claude-haiku-4-5', undefined, undefined],
])('shows the URL field the composer model %s runs on, whatever the decider', async (composer_model, endpoint, ollama) => {
  await show(fresh({ composer_model, openai_base_url: 'https://llm.example.test/v1', ollama_url: 'http://localhost:11434' }));
  const value = (label: string) => (screen.queryByLabelText(label) as HTMLInputElement | null)?.value;
  expect([value('Endpoint URL'), value('Ollama server URL')]).toEqual([endpoint, ollama]);
});

it('marks the URL an Ollama composer model lacks', async () => {
  const noComposerUrl = { code: 'composer_not_ready' as const, path: 'ollama_url',
    message: 'The rule composer model, ollama:llama3.2, needs the URL of your Ollama server. Until it is set, Describe it, Rewrite with AI and Suggest from my mail do not work.' };
  await show(fresh({ composer_model: 'ollama:llama3.2', warnings: [noComposerUrl] }));
  const warning = screen.getByRole('status');
  expect(warning.textContent).toBe(noComposerUrl.message);
  expect(screen.getByLabelText('Ollama server URL').getAttribute('aria-describedby')).toBe(warning.id);
});

// The warnings a daemon with no keys answers (recorded from a real PATCH).
const noUrl = { code: 'decider_not_ready' as const, message: 'The ollama decision model needs the URL of your Ollama server. Until it is set, rules that need a model are passed over.', path: 'ollama_url' };
const noKey = { code: 'decider_not_ready' as const, message: 'The jev decision model needs an OpenRouter API key. Until it is set, rules that need a model are passed over.', path: 'keys.openrouter_api_key' };

it('shows the warning of a saved decider that cannot work, marks the URL it names, and clears both when it is filled in', async () => {
  await show(fresh());
  expect(screen.queryByRole('status')).toBeNull();
  expect(screen.queryByText('Needed')).toBeNull();

  routes['PATCH /api/settings'] = [200, fresh({ decider: 'ollama', decider_model: 'llama3.2', warnings: [noUrl] })];
  await fireEvent.change(screen.getByLabelText('Decision model'), { target: { value: 'ollama' } });
  await fireEvent.input(screen.getByLabelText('Decision model name'), { target: { value: 'llama3.2' } });
  await fireEvent.change(screen.getByLabelText('Decision model name'));
  const warning = await screen.findByRole('status');
  expect(warning.textContent).toBe(noUrl.message);
  const field = screen.getByLabelText('Ollama server URL');
  expect(field.getAttribute('aria-describedby')).toBe(warning.id);
  expect(field.parentElement!.textContent).toContain('Needed');

  routes['PATCH /api/settings'] = [200, fresh({ decider: 'ollama', decider_model: 'llama3.2', ollama_url: 'http://localhost:11434' })];
  await fireEvent.change(field, { target: { value: 'http://localhost:11434' } });
  await waitFor(() => expect(screen.queryByRole('status')).toBeNull());
  expect(field.getAttribute('aria-describedby')).toBeNull();
  expect(screen.queryByText('Needed')).toBeNull();
});

it('marks the provider key a warning names, and no other', async () => {
  await show(fresh({ warnings: [noKey] }));
  const warning = screen.getByRole('status');
  expect(warning.textContent).toBe(noKey.message);
  expect(warning.compareDocumentPosition(screen.getByLabelText('Decision model name')) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  expect(screen.getByLabelText(/^OpenRouter API key/).getAttribute('aria-describedby')).toBe(warning.id);
  expect(screen.getByLabelText(/^Anthropic API key/).getAttribute('aria-describedby')).toBeNull();
  expect(screen.getAllByText('Needed')).toHaveLength(1);
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

it('links every key row to its provider guide in a new tab, both Cloudflare rows to the same one', async () => {
  await show(fresh());
  const guide = (label: RegExp) => screen.getByLabelText(label).closest('label')!.querySelector('a')!;
  const want: [RegExp, string][] = [
    [/^OpenRouter API key/, 'https://openrouter.ai/docs/api-reference/authentication'],
    [/^Cloudflare account ID/, 'https://developers.cloudflare.com/workers-ai/get-started/rest-api/'],
    [/^Cloudflare API token/, 'https://developers.cloudflare.com/workers-ai/get-started/rest-api/'],
    [/^Anthropic API key/, 'https://platform.claude.com/docs/en/api/overview'],
    [/^OpenAI API key/, 'https://developers.openai.com/api/docs/quickstart'],
  ];
  for (const [label, href] of want) {
    const a = guide(label);
    expect(a.textContent).toBe('How to get this');
    expect(a.getAttribute('href')).toBe(href);
    expect(a.getAttribute('target')).toBe('_blank');
    expect(a.getAttribute('rel')).toBe('noopener noreferrer');
  }
});

it('shows the Ollama setup guide only while Ollama is the decision model', async () => {
  await show(fresh());
  expect(screen.queryByRole('link', { name: 'Setup guide' })).toBeNull();
  await fireEvent.change(screen.getByLabelText('Decision model'), { target: { value: 'ollama' } });
  const a = screen.getByRole('link', { name: 'Setup guide' });
  expect(a.getAttribute('href')).toBe('https://docs.ollama.com/quickstart');
  expect(a.getAttribute('target')).toBe('_blank');
  expect(a.getAttribute('rel')).toBe('noopener noreferrer');
});

it('keeps the keys nothing uses under a collapsed "Other providers" until it is opened', async () => {
  await show(fresh({ fallback_model: '', composer_model: '', keys: { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' } }));
  const other = screen.getByText('Other providers').closest('details')!;
  expect(other.open).toBe(false);
  const inOther = (label: RegExp) => other.contains(screen.getByLabelText(label));
  expect(inOther(/^OpenRouter API key/)).toBe(false);
  expect([/^Cloudflare account ID/, /^Cloudflare API token/, /^Anthropic API key/, /^OpenAI API key/].map(inOther)).toEqual([true, true, true, true]);
  await fireEvent.click(screen.getByText('Other providers'));
  expect(other.open).toBe(true);
});

it.each<[name: 'trash_to_folder' | 'leave_own_mail', label: RegExp, help: string]>([
  ['trash_to_folder', /^Send trashed mail to MailRules Trash/, "providers empty Trash on their own; MailRules' folder is never emptied, so mail trashed by mistake can still be found"],
  ['leave_own_mail', /^Leave my own emails alone/, "mail sent from this mailbox's own address is never sorted, trashed or sent to the AI"],
])('turns %s off and on, and snaps the box back when the daemon refuses', async (name, label, help) => {
  await show(fresh());
  const box = screen.getByRole('checkbox', { name: label }) as HTMLInputElement;
  expect(box.closest('label')!.textContent).toContain(help);

  routes['PATCH /api/settings'] = [200, fresh({ [name]: false })];
  await fireEvent.click(box);
  await waitFor(() => expect(settings.value[name]).toBe(false));
  expect(box.checked).toBe(false);

  routes['PATCH /api/settings'] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  await fireEvent.click(box);
  await waitFor(() => expect(patches()).toEqual([{ [name]: false }, { [name]: true }]));
  await waitFor(() => expect(box.checked).toBe(false));
  expect(settings.value[name]).toBe(false);
});

describe('the Anthropic workspace', () => {
  const lookups = () => fetchMock.mock.calls.filter((c) => c[0] === '/api/settings/anthropic-workspaces').length;
  const several = { status: 'several', workspaces: [{ id: 'wrkspc_default', name: 'Default' }, { id: 'wrkspc_mail', name: 'Mail' }] };
  const needed = { code: 'anthropic_workspace_needed' as const, path: 'anthropic_workspace_id',
    message: "Your Claude key covers your whole organisation, so Claude refuses MailRules' requests until a workspace is chosen. Choose the Anthropic workspace under the key." };

  it('stays out of the way for a key that needs none, and for no key at all', async () => {
    await show(fresh());
    await waitFor(() => expect(lookups()).toBe(1));
    expect(screen.queryByLabelText('Anthropic workspace')).toBeNull();

    fetchMock.mockClear();
    cleanup();
    await show(fresh({ keys: { ...fresh().keys, anthropic_api_key: 'none' } }));
    expect(screen.queryByLabelText('Anthropic workspace')).toBeNull();
    expect(lookups()).toBe(0);
  });

  it('shows the workspace found automatically by name and ID, without asking again', async () => {
    await show(fresh({ anthropic_workspace_id: 'wrkspc_default', anthropic_workspace_name: 'Default', anthropic_workspace_found: true }));
    const row = screen.getByLabelText('Anthropic workspace').closest('div.flex-col')!;
    expect(row.textContent).toContain('Found automatically');
    expect(row.textContent).toContain('Default · wrkspc_default');
    const guide = screen.getByText('How to find it');
    expect([guide.getAttribute('href'), guide.getAttribute('target'), guide.getAttribute('rel')]).toEqual(['https://platform.claude.com/settings/workspaces', '_blank', 'noopener noreferrer']);
    expect(lookups()).toBe(0);
  });

  it('lists several to pick from, and stores the one picked', async () => {
    routes['GET /api/settings/anthropic-workspaces'] = [200, several];
    await show(fresh());
    const picker = (await screen.findByLabelText('Choose a workspace')) as HTMLSelectElement;
    expect([...picker.options].map((o) => o.textContent)).toEqual(['Choose a workspace', 'Default (wrkspc_default)', 'Mail (wrkspc_mail)']);
    expect(screen.queryByText('Found automatically')).toBeNull();

    routes['PATCH /api/settings'] = [200, fresh({ anthropic_workspace_id: 'wrkspc_mail', anthropic_workspace_name: 'Mail' })];
    await fireEvent.change(picker, { target: { value: 'wrkspc_mail' } });
    await waitFor(() => expect(settings.value.anthropic_workspace_id).toBe('wrkspc_mail'));
    expect(patches()).toEqual([{ anthropic_workspace_id: 'wrkspc_mail' }]);
    expect(screen.getByLabelText('Anthropic workspace').closest('div.flex-col')!.textContent).toContain('Mail · wrkspc_mail');
    expect(picker.value).toBe('wrkspc_mail');
  });

  it('says when the workspaces could not be looked up, takes a typed ID, and shows a refusal beside the field', async () => {
    routes['GET /api/settings/anthropic-workspaces'] = [200, { status: 'failed', workspaces: [] }];
    await show(fresh());
    expect(await screen.findByText('MailRules could not look up your workspaces. Type the workspace ID.')).toBeTruthy();
    const field = screen.getByLabelText('Anthropic workspace') as HTMLInputElement;

    const refusal = 'A workspace ID starts with wrkspc_ followed by letters and digits. Copy it from the ID column of Settings → Workspaces in the Claude Console.';
    routes['PATCH /api/settings'] = [400, { error: { code: 'invalid_input', message: refusal, path: 'anthropic_workspace_id' } }];
    await fireEvent.input(field, { target: { value: 'Default' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save Anthropic workspace' }));
    expect((await screen.findByRole('alert')).textContent).toBe(refusal);
    expect(field.getAttribute('aria-invalid')).toBe('true');

    routes['PATCH /api/settings'] = [200, fresh({ anthropic_workspace_id: 'wrkspc_01Jw' })];
    await fireEvent.input(field, { target: { value: ' wrkspc_01Jw ' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Save Anthropic workspace' }));
    await waitFor(() => expect(screen.queryByRole('alert')).toBeNull());
    expect(field.value).toBe('');
    expect(screen.queryByText(/could not look up/)).toBeNull();
    expect(screen.getByLabelText('Anthropic workspace').closest('div.flex-col')!.textContent).toContain('wrkspc_01Jw');

    routes['PATCH /api/settings'] = [200, fresh()];
    await fireEvent.click(screen.getByRole('button', { name: 'Remove Anthropic workspace' }));
    await waitFor(() => expect(settings.value.anthropic_workspace_id).toBe(''));
    expect(patches()).toEqual([{ anthropic_workspace_id: 'Default' }, { anthropic_workspace_id: 'wrkspc_01Jw' }, { anthropic_workspace_id: null }]);
  });

  it('points the warning of a refused key at the field', async () => {
    routes['GET /api/settings/anthropic-workspaces'] = [200, several];
    await show(fresh({ warnings: [needed] }));
    const warning = screen.getByRole('status');
    expect(warning.textContent).toBe(needed.message);
    const field = await screen.findByLabelText('Anthropic workspace');
    expect(field.getAttribute('aria-describedby')).toBe(warning.id);
    expect(field.closest('div.flex-col')!.textContent).toContain('Needed');
  });

  it('reads the settings afresh on opening, so a refusal since the shell loaded them shows', async () => {
    routes['GET /api/settings'] = [200, fresh()];
    await load();
    routes['GET /api/settings'] = [200, fresh({ warnings: [needed] })];
    routes['GET /api/settings/anthropic-workspaces'] = [200, { status: 'failed', workspaces: [] }];
    render(Settings);
    expect((await screen.findByRole('status')).textContent).toBe(needed.message);
    expect((await screen.findByLabelText('Anthropic workspace')).getAttribute('aria-describedby')).toBe('warn-anthropic_workspace_id');
  });

  it('looks the workspace up again when the Claude key is replaced', async () => {
    await show(fresh());
    await waitFor(() => expect(lookups()).toBe(1));
    routes['PATCH /api/settings'] = [200, fresh()];
    routes['GET /api/settings/anthropic-workspaces'] = [200, several];
    await fireEvent.input(screen.getByLabelText(/^Anthropic API key/), { target: { value: 'sk-ant-new' } });
    await fireEvent.submit(screen.getByLabelText(/^Anthropic API key/).closest('form')!);
    expect(await screen.findByLabelText('Choose a workspace')).toBeTruthy();
    expect(lookups()).toBe(2);
  });
});

describe('Summary email', () => {
  const summary = (over: Partial<Saved['summary']> = {}): Saved['summary'] => ({
    ...fresh().summary, to: 'me@example.test', to_default: 'me@example.test', smtp: { configured: true, missing: [] }, ...over,
  });
  const card = () => within(screen.getByRole('region', { name: 'Summary email' }));
  const zone = Intl.DateTimeFormat().resolvedOptions().timeZone;

  it('is switched off, with the settings to set, while no mail server is configured; the preview still works', async () => {
    await show(fresh({ summary: summary({ smtp: { configured: false, missing: ['MAILRULES_SMTP_HOST', 'MAILRULES_SMTP_FROM'] } }) }));
    expect((card().getByRole('checkbox', { name: /^Send me a summary/ }) as HTMLInputElement).disabled).toBe(true);
    expect(card().getByText('To send it, set MAILRULES_SMTP_HOST and MAILRULES_SMTP_FROM, then restart MailRules.')).toBeTruthy();
    expect((card().getByRole('button', { name: 'Send a test email' }) as HTMLButtonElement).disabled).toBe(true);
    expect((card().getByRole('button', { name: 'Show preview' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('switching on saves enabled with the browser time zone, and a refusal snaps the switch back', async () => {
    await show(fresh({ summary: summary() }));
    const box = card().getByRole('checkbox', { name: /^Send me a summary/ }) as HTMLInputElement;
    routes['PATCH /api/settings'] = [200, fresh({ summary: summary({ enabled: true, next_at: 1_800_000_000 }) })];
    await fireEvent.click(box);
    await waitFor(() => expect(patches()).toEqual([{ summary: { enabled: true, time_zone: zone } }]));
    await card().findByText(/^Next one: /);

    routes['PATCH /api/settings'] = [409, { error: { code: 'smtp_not_configured', message: 'Set MAILRULES_SMTP_HOST first.', path: 'summary.enabled' } }];
    await fireEvent.click(box);
    await fireEvent.click(box);
    await waitFor(() => expect(patches()).toHaveLength(3));
    await waitFor(() => expect(box.checked).toBe(true));
  });

  it('a refused switch-on unticks the box again', async () => {
    await show(fresh({ summary: summary() }));
    routes['PATCH /api/settings'] = [409, { error: { code: 'smtp_not_configured', message: 'No mail server.', path: 'summary.enabled' } }];
    const box = card().getByRole('checkbox', { name: /^Send me a summary/ }) as HTMLInputElement;
    await fireEvent.click(box);
    await waitFor(() => expect(patches()).toHaveLength(1));
    await waitFor(() => expect(box.checked).toBe(false));
  });

  it('weekly shows the day, and frequency, day, time and address each save', async () => {
    await show(fresh({ summary: summary() }));
    expect(card().queryByLabelText('On')).toBeNull();
    routes['PATCH /api/settings'] = [200, fresh({ summary: summary({ frequency: 'weekly' }) })];
    await fireEvent.change(card().getByLabelText('How often'), { target: { value: 'weekly' } });
    const day = await card().findByLabelText('On');
    await fireEvent.change(day, { target: { value: 'friday' } });
    await fireEvent.change(card().getByLabelText('At'), { target: { value: '18:30' } });
    const to = card().getByLabelText('Send to') as HTMLInputElement;
    expect(to.placeholder).toBe('me@example.test');
    await fireEvent.change(to, { target: { value: 'other@example.test' } });
    await fireEvent.change(to, { target: { value: '' } });
    await waitFor(() => expect(patches()).toHaveLength(5));
    expect(patches()).toEqual([
      { summary: { frequency: 'weekly', time_zone: zone } },
      { summary: { weekday: 'friday', time_zone: zone } },
      { summary: { time: '18:30', time_zone: zone } },
      { summary: { to: 'other@example.test', time_zone: zone } },
      { summary: { to: null, time_zone: zone } },
    ]);
  });

  it('sends a test email and says where, or shows why it failed', async () => {
    await show(fresh({ summary: summary() }));
    routes['POST /api/summary/test'] = [200, { to: 'me@example.test', subject: 'x' }];
    await fireEvent.click(card().getByRole('button', { name: 'Send a test email' }));
    expect((await card().findByRole('status')).textContent).toBe('Sent to me@example.test.');
    expect(fetchMock.mock.calls.some((c) => c[0] === '/api/summary/test' && c[1].method === 'POST')).toBe(true);

    routes['POST /api/summary/test'] = [502, { error: { code: 'send_failed', message: 'The mail server refused the login.' } }];
    await fireEvent.click(card().getByRole('button', { name: 'Send a test email' }));
    expect((await card().findByRole('alert')).textContent).toBe('The mail server refused the login.');
  });

  it('shows the preview with its subject in a sandboxed frame', async () => {
    await show(fresh({ summary: summary() }));
    routes['GET /api/summary/preview'] = [200, { to: 'me@example.test', subject: '142 sorted, 3 need you', text: '', html: '<html><body>Hi</body></html>', period_start: 0, period_end: 1, dry_run: true }];
    await fireEvent.click(card().getByRole('button', { name: 'Show preview' }));
    const frame = (await card().findByTitle('Summary email preview')) as HTMLIFrameElement;
    expect(frame.getAttribute('src')).toMatch(/^\/api\/summary\/preview\.html\?at=\d+$/);
    expect(frame.getAttribute('sandbox')).toBe('allow-popups allow-popups-to-escape-sandbox');
    expect(card().getByText('142 sorted, 3 need you')).toBeTruthy();
    expect(card().getByText(/^Dry-run is on/)).toBeTruthy();
    await fireEvent.click(card().getByRole('button', { name: 'Hide preview' }));
    expect(card().queryByTitle('Summary email preview')).toBeNull();
  });
});
