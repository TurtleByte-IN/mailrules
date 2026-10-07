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

/** The providers the rule composer can run on. */
export type ComposerProvider = 'anthropic' | 'openai' | 'ollama';

/**
 * The provider the rule composer runs on, read the way the daemon reads `composer_model`
 * (config.SplitComposerModel): a bare name is a Claude model, `anthropic:`, `openai:` or
 * `ollama:` names the provider. Undefined when it is empty or names another provider (the
 * daemon refuses both).
 */
export function composerProvider(model: string): ComposerProvider | undefined {
  const m = model.trim();
  if (!m) return undefined;
  const at = m.indexOf(':');
  if (at < 0) return 'anthropic';
  const name = m.slice(0, at);
  return name === 'anthropic' || name === 'openai' || name === 'ollama' ? name : undefined;
}

export type KeysInUseInput = Pick<Settings, 'decider' | 'fallback_model' | 'composer_model' | 'keys'> & {
  /** The `model` of every rule. */
  ruleModels: string[];
};

/**
 * The key fields to show up front: those whose provider serves the decision model, the
 * fallback (always Anthropic, off when empty), the rule composer (its provider's key, none
 * for Ollama) or a model a rule names for itself, plus every key holding a value saved here,
 * so a stored secret is never out of sight and can always be removed.
 */
export function keysInUse(i: KeysInUseInput): Set<KeyName> {
  const used = new Set<KeyName>(credentials[i.decider]);
  if (i.fallback_model.trim()) used.add('anthropic_api_key');
  const composer = composerProvider(i.composer_model);
  if (composer) credentials[composer].forEach((k) => used.add(k));
  for (const m of i.ruleModels) ruleCredentials(m).forEach((k) => used.add(k));
  for (const [k, source] of Object.entries(i.keys)) if (source === 'stored') used.add(k as KeyName);
  return used;
}
