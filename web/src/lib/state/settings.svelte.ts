import * as settingsApi from '../api/settings';
import { flash } from './toast.svelte';

export const settings = $state<{ value: settingsApi.Settings }>({ value: { dryRun: true } });

export async function load() {
  settings.value = await settingsApi.get();
}

export async function patch(p: Partial<settingsApi.Settings>) {
  settings.value = await settingsApi.patch(p);
}

export async function toggleDryRun() {
  await patch({ dryRun: !settings.value.dryRun });
  flash(
    settings.value.dryRun
      ? 'Dry-run on: nothing in your mailbox will change'
      : 'Live: MailRules is sorting your mail again',
  );
}
