// Keys are write-only: PATCH takes them, GET only says which are set.
import { api } from './client';
import type { components } from './schema';

export type Settings = components['schemas']['Settings'];
export type SettingsPatch = components['schemas']['SettingsPatch'];
export type Decider = components['schemas']['Decider'];
export type KeyName = keyof components['schemas']['ProviderKeys'];
export type UrlName = 'openai_base_url' | 'ollama_url';

export const get = () => api<Settings>('GET', '/settings');
export const patch = (p: SettingsPatch) => api<Settings>('PATCH', '/settings', p);
