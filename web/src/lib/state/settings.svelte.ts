import * as settingsApi from '../api/settings';
import { flash } from './toast.svelte';

// Shown until the first load: the daemon's defaults (docs/backend-plan.md → Configuration).
export const settings = $state<{ value: settingsApi.Settings }>({
  value: {
    dryRun: true,
    decider: 'jev',
    fallback: true,
    escalateBelow: 0.75,
    retentionDays: 30,
    keys: { openrouter: false, anthropic: false },
  },
});

export async function load() {
  settings.value = await settingsApi.get();
}

export async function patch(p: Partial<settingsApi.Settings>) {
  settings.value = await settingsApi.patch(p);
}

/** Sends a model key once. Only "is set" comes back; the key is never kept here. */
export async function setKey(name: settingsApi.KeyName, secret: string, label: string) {
  settings.value = await settingsApi.setKey(name, secret);
  flash(label + ' saved');
}

export async function toggleDryRun() {
  await patch({ dryRun: !settings.value.dryRun });
  flash(
    settings.value.dryRun
      ? 'Dry-run on: nothing in your mailbox will change'
      : 'Live: MailRules is sorting your mail again',
  );
}
