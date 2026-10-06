// DEMO: GET/PATCH /api/settings are not in api/openapi.yaml yet (backend M7).
// Keys are write-only: PATCH takes one, GET only says whether it is set.
import { fake } from './demo';

/** MAILRULES_DECIDER values the prototype offers. */
export type Decider = 'jev' | 'clef' | 'anthropic' | 'ollama';
export type KeyName = 'openrouter' | 'anthropic';

export interface Settings {
  /** On by default: the daemon logs what it would do and changes nothing. */
  dryRun: boolean;
  decider: Decider;
  /** Ask Claude Haiku when the decider is unsure. */
  fallback: boolean;
  /** Decider confidence below this escalates, 0 to 1. */
  escalateBelow: number;
  /** Days email snippets are kept. */
  retentionDays: number;
  /** Whether each model key is set. The key itself never comes back. */
  keys: Record<KeyName, boolean>;
  /** Self-host only. */
  install?: { version: string; dataDir: string; listen: string };
}

let settings: Settings = {
  dryRun: true,
  decider: 'jev',
  fallback: true,
  escalateBelow: 0.75,
  retentionDays: 30,
  keys: { openrouter: true, anthropic: false },
  install: { version: '0.4.0', dataDir: './data', listen: '127.0.0.1:8080' },
};

export const get = () => fake(settings);
export const patch = (p: Partial<Settings>) => fake((settings = { ...settings, ...p }));

/** The demo records that a key was given and drops the key. */
export const setKey = (name: KeyName, _secret: string) =>
  fake((settings = { ...settings, keys: { ...settings.keys, [name]: true } }));
