import type { Decider, KeyName, Settings } from '../../lib/api/settings';

/** The credentials each decider name needs (`jev`, `clef`, `anthropic`, `openai`, `ollama`; Ollama needs none). */
const credentials: Record<Decider, KeyName[]> = {
  jev: ['openrouter_api_key'],
  clef: ['cloudflare_account_id', 'cloudflare_api_token'],
  anthropic: ['anthropic_api_key'],
  openai: ['openai_api_key'],
  ollama: [],
};

/** A rule's own `model` is a decider name or `name:model`; empty means the default. Unknown names need nothing. */
const ruleCredentials = (model: string): KeyName[] => credentials[model.split(':')[0].trim().toLowerCase() as Decider] ?? [];

export type KeysInUseInput = Pick<Settings, 'decider' | 'fallback_model' | 'composer_model' | 'keys'> & {
  /** The `model` of every rule. */
  ruleModels: string[];
};

/**
 * The key fields to show up front: those whose provider serves the decision model, the
 * fallback (always Anthropic, off when empty), the rule composer (always Anthropic, off when
 * empty) or a model a rule names for itself, plus every key holding a value saved here, so a
 * stored secret is never out of sight and can always be removed.
 */
export function keysInUse(i: KeysInUseInput): Set<KeyName> {
  const used = new Set<KeyName>(credentials[i.decider]);
  if (i.fallback_model.trim()) used.add('anthropic_api_key');
  if (i.composer_model.trim()) used.add('anthropic_api_key');
  for (const m of i.ruleModels) ruleCredentials(m).forEach((k) => used.add(k));
  for (const [k, source] of Object.entries(i.keys)) if (source === 'stored') used.add(k as KeyName);
  return used;
}
