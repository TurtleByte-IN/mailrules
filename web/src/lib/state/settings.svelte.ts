import { ApiError } from '../api/client';
import * as settingsApi from '../api/settings';
import { flash } from './toast.svelte';

// `value` is a placeholder until `loaded`: dry-run on, so the shell never says "live"
// before the daemon has. Screens that show the other fields wait for `loaded`. Its
// `limits` are 0, unknown: the number boxes then ask only for a whole number from 1.
export const settings = $state<{ value: settingsApi.Settings; loaded: boolean; error: string }>({
  value: {
    dry_run: true,
    decider: 'jev',
    decider_model: '',
    fallback_model: '',
    fallback_active: false,
    fallback_note: '',
    composer_model: '',
    escalate_below: 0,
    min_confidence: 0,
    retention_days: 0,
    trash_to_folder: true,
    leave_own_mail: true,
    openai_base_url: '',
    ollama_url: '',
    anthropic_workspace_id: '',
    anthropic_workspace_name: '',
    anthropic_workspace_found: false,
    keys: { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' },
    warnings: [],
    server: { version: '', data_dir: '', listen: '', mode: 'selfhost' },
    limits: { test_default: 0, test_max: 0, check_max: 0 },
    features: { notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false, suggest: false },
    summary: { enabled: false, frequency: 'daily', weekday: 'monday', time: '08:00', time_zone: 'UTC', to: '', to_default: '', smtp: { configured: false, missing: [] }, last_sent_at: null, next_at: null },
  },
  loaded: false,
  error: '',
});

const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

/** The id of the warning that names this setting (as SettingsPatch spells it), when there is one. */
export const warningId = (path: string) => (settings.value.warnings.some((w) => w.path === path) ? 'warn-' + path : undefined);

// The Claude workspace. Only a Claude key that covers a whole organisation needs one; `lookup`
// is the daemon's answer for the key in force, or null while there is none.
export const workspaces = $state<{ lookup: settingsApi.AnthropicWorkspaces | null }>({ lookup: null });

/** Asks the daemon which workspace the Claude key needs, while one is in force and none is set. */
export async function findWorkspace() {
  workspaces.lookup = null;
  if (settings.value.keys.anthropic_api_key === 'none' || settings.value.anthropic_workspace_id) return;
  try {
    workspaces.lookup = await settingsApi.anthropicWorkspaces();
  } catch {
    // The control stays hidden; the field is still reachable through a warning.
  }
}

// Bumped by every load and every saved change: an answer to an older request is dropped, so a
// slow load cannot put older values, or an error, back over newer ones.
let generation = 0;

export async function load() {
  const mine = ++generation;
  try {
    const value = await settingsApi.get();
    if (mine !== generation) return;
    settings.value = value;
    settings.loaded = true;
    settings.error = '';
  } catch (e) {
    if (mine === generation) settings.error = message(e);
  }
}

/** Keeps the settings a saved change answered with, and makes any load still on its way stale. */
function keep(value: settingsApi.Settings) {
  generation++;
  settings.value = value;
}

/** Resolves false, after a toast with the daemon's reason, when the change was refused. */
export async function patch(p: settingsApi.SettingsPatch) {
  try {
    keep(await settingsApi.patch(p));
    return true;
  } catch (e) {
    flash(message(e));
    return false;
  }
}

/**
 * Saves the URL a decider talks to; an empty one forgets the stored value (null), which puts the
 * environment's back. Resolves the daemon's reason when it refuses the URL, to show beside the
 * field, and '' otherwise.
 */
export async function setUrl(name: settingsApi.UrlName, url: string) {
  try {
    keep(await settingsApi.patch({ [name]: url || null }));
  } catch (e) {
    if (e instanceof ApiError && e.path === name) return e.message;
    flash(message(e));
  }
  return '';
}

/**
 * Saves the Claude workspace: an ID, or null to forget the stored one. Resolves the daemon's
 * reason when it refuses the ID, to show beside the field, and '' otherwise.
 */
export async function setWorkspace(id: string | null) {
  try {
    keep(await settingsApi.patch({ anthropic_workspace_id: id }));
  } catch (e) {
    if (e instanceof ApiError && e.path === 'anthropic_workspace_id') return e.message;
    flash(message(e));
  }
  return '';
}

/** Sends a provider key once; an empty one removes the stored key. Only "is set" comes back; the key is never kept here. */
export async function setKey(name: settingsApi.KeyName, secret: string, label: string) {
  if (!(await patch({ keys: { [name]: secret } }))) return;
  if (secret) flash(label + ' saved');
  else flash(settings.value.keys[name] === 'environment' ? `Stored ${label} removed; the one in the environment is in use` : label + ' removed');
}

export async function toggleDryRun() {
  if (!(await patch({ dry_run: !settings.value.dry_run }))) return;
  flash(
    settings.value.dry_run
      ? 'Dry-run on: nothing in your mailbox will change'
      : 'Live: MailRules is sorting your mail again',
  );
}
