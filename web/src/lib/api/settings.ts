// Keys are write-only: PATCH takes them, GET only says which are set.
import { api } from './client';
import type { components } from './schema';

export type Settings = components['schemas']['Settings'];
export type SettingsPatch = components['schemas']['SettingsPatch'];
export type Decider = components['schemas']['Decider'];
export type KeyName = keyof components['schemas']['ProviderKeys'];
export type UrlName = 'openai_base_url' | 'ollama_url';

// Mirrors the daemon's actions.TrashFolder (internal/actions/executor.go): where a trash
// action moves mail while `trash_to_folder` is on.
export const TRASH_FOLDER = 'MailRules Trash';

export const get = () => api<Settings>('GET', '/settings');
export const patch = (p: SettingsPatch) => api<Settings>('PATCH', '/settings', p);
