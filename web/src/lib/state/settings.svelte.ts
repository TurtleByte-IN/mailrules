import { ApiError } from '../api/client';
import * as settingsApi from '../api/settings';
import { flash } from './toast.svelte';

// `value` is a placeholder until `loaded`: dry-run on, so the shell never says "live"
// before the daemon has. Screens that show the other fields wait for `loaded`.
export const settings = $state<{ value: settingsApi.Settings; loaded: boolean; error: string }>({
  value: {
    dry_run: true,
    decider: 'jev',
    decider_model: '',
    fallback_model: '',
    composer_model: '',
    escalate_below: 0,
    min_confidence: 0,
    retention_days: 0,
    trash_to_folder: true,
    openai_base_url: '',
    ollama_url: '',
    anthropic_workspace_id: '',
    anthropic_workspace_name: '',
    anthropic_workspace_found: false,
    keys: { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' },
    warnings: [],
    server: { version: '', data_dir: '', listen: '', mode: 'selfhost' },
    features: { digest: false, notifications: false, timed_actions: false, draft_replies: false, billing: false, unsubscribe: false, oauth_providers: false },
  },
  loaded: false,
  error: '',
});

const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

export async function load() {
  try {
    settings.value = await settingsApi.get();
    settings.loaded = true;
    settings.error = '';
  } catch (e) {
    settings.error = message(e);
  }
}

/** Resolves false, after a toast with the daemon's reason, when the change was refused. */
export async function patch(p: settingsApi.SettingsPatch) {
  try {
    settings.value = await settingsApi.patch(p);
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
    settings.value = await settingsApi.patch({ [name]: url || null });
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
    settings.value = await settingsApi.patch({ anthropic_workspace_id: id });
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
