// Keys are write-only: PATCH takes them, GET only says which are set.
import { api } from './client';
import type { components } from './schema';

export type Settings = components['schemas']['Settings'];
export type SettingsPatch = components['schemas']['SettingsPatch'];
export type Decider = components['schemas']['Decider'];
export type KeyName = keyof components['schemas']['ProviderKeys'];
export type UrlName = 'openai_base_url' | 'ollama_url';
export type AnthropicWorkspaces = components['schemas']['AnthropicWorkspaces'];

// Mirrors the daemon's actions.TrashFolder (internal/actions/executor.go): where a trash
// action moves mail while `trash_to_folder` is on.
export const TRASH_FOLDER = 'MailRules Trash';

export const get = () => api<Settings>('GET', '/settings');
export const patch = (p: SettingsPatch) => api<Settings>('PATCH', '/settings', p);
/** Which workspace the Claude key in force needs; the daemon asks Anthropic only while it does not know. */
export const anthropicWorkspaces = () => api<AnthropicWorkspaces>('GET', '/settings/anthropic-workspaces');

export type SummarySettings = Settings['summary'];
export type SummaryPatch = components['schemas']['SummaryPatch'];
export type SummaryPreview = components['schemas']['SummaryPreview'];
export type SummaryTestResult = components['schemas']['SummaryTestResult'];

/** The summary email as the next one would be, without sending it; works without SMTP. */
export const previewSummary = () => api<SummaryPreview>('GET', '/summary/preview');
/** Sends a summary of the last day now; 409 without SMTP, 502 when the mail server refused it. */
export const sendTestSummary = () => api<SummaryTestResult>('POST', '/summary/test');
