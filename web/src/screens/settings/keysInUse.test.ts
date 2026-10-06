import { expect, it } from 'vitest';
import type { KeyName } from '../../lib/api/settings';
import { keysInUse, type KeysInUseInput } from './keysInUse';

const none = { openrouter_api_key: 'none', cloudflare_account_id: 'none', cloudflare_api_token: 'none', anthropic_api_key: 'none', openai_api_key: 'none' } as const;
const base: KeysInUseInput = { decider: 'jev', fallback_model: '', composer_model: '', keys: none, ruleModels: [] };

it.each<[string, Partial<KeysInUseInput>, KeyName[]]>([
  ['decision model only: Jev', {}, ['openrouter_api_key']],
  ['decision model only: Clef needs both Cloudflare values', { decider: 'clef' }, ['cloudflare_account_id', 'cloudflare_api_token']],
  ['decision model only: Anthropic', { decider: 'anthropic' }, ['anthropic_api_key']],
  ['decision model only: OpenAI-compatible', { decider: 'openai' }, ['openai_api_key']],
  ['decision model only: Ollama has no key', { decider: 'ollama' }, []],
  ['a fallback model adds Anthropic', { fallback_model: 'claude-haiku-4-5' }, ['openrouter_api_key', 'anthropic_api_key']],
  ['a composer model adds Anthropic', { composer_model: 'claude-haiku-4-5' }, ['openrouter_api_key', 'anthropic_api_key']],
  ['an empty fallback and composer add nothing', { decider: 'ollama', fallback_model: ' ', composer_model: '' }, []],
  ['a rule naming Clef adds both Cloudflare values', { ruleModels: ['', 'clef'] }, ['openrouter_api_key', 'cloudflare_account_id', 'cloudflare_api_token']],
  ['a rule naming name:model counts by name', { ruleModels: ['openai:gpt-4o-mini'] }, ['openrouter_api_key', 'openai_api_key']],
  ['a rule naming Ollama or an unknown name adds nothing', { ruleModels: ['ollama:llama3.2', 'mystery:x'] }, ['openrouter_api_key']],
  ['a saved value stays visible with nothing using it', { decider: 'ollama', keys: { ...none, cloudflare_api_token: 'stored' } }, ['cloudflare_api_token']],
  ['a key set by the environment is not forced visible', { decider: 'ollama', keys: { ...none, openai_api_key: 'environment' } }, []],
])('%s', (_name, over, want) => {
  expect([...keysInUse({ ...base, ...over })].sort()).toEqual([...want].sort());
});
