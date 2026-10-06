import { expect, it } from 'vitest';
import * as settingsApi from '../api/settings';
import { load, patch, setKey, settings, toggleDryRun } from './settings.svelte';
import { toast } from './toast.svelte';

it.each<[string, Partial<settingsApi.Settings>]>([
  ['decider', { decider: 'ollama' }],
  ['fallback', { fallback: false }],
  ['threshold', { escalateBelow: 0.6 }],
  ['retention', { retentionDays: 90 }],
])('patches %s and keeps the rest', async (_name, change) => {
  await load();
  const before = { ...settings.value };
  await patch(change);
  expect(settings.value).toEqual({ ...before, ...change });
  expect(await settingsApi.get()).toEqual(settings.value);
});

it('toggles dry-run both ways with the matching toast', async () => {
  await load();
  await toggleDryRun();
  expect(settings.value.dryRun).toBe(false);
  expect(toast.text).toBe('Live: MailRules is sorting your mail again');
  await toggleDryRun();
  expect(settings.value.dryRun).toBe(true);
  expect(toast.text).toBe('Dry-run on: nothing in your mailbox will change');
});

it('records that a key is set without keeping the key', async () => {
  await load();
  expect(settings.value.keys.anthropic).toBe(false);
  await setKey('anthropic', 'sk-ant-secret-123', 'Anthropic API key');

  expect(settings.value.keys.anthropic).toBe(true);
  expect(toast.text).toBe('Anthropic API key saved');
  expect(JSON.stringify([settings, await settingsApi.get(), { ...localStorage }, { ...sessionStorage }])).not.toContain('sk-ant-secret-123');
});
