import { settings } from './settings.svelte';

// What the user typed; undefined until they type, which shows the daemon's own default.
const typed = $state<{ value: number | null | undefined }>({ value: undefined });

/**
 * How many emails the rule tester reads. One number for both screens that start a test, kept while the
 * app is open and forgotten when it is closed. Until the user changes it, it is what the daemon reads
 * when it is not told (`limits.test_default` in the settings). null is an empty box, as it is while the
 * settings are not loaded yet.
 */
export const testLimit = {
  get value(): number | null {
    return typed.value === undefined ? settings.value.limits.test_default || null : typed.value;
  },
  set value(n: number | null) {
    typed.value = n;
  },
};

/**
 * What is wrong with a number of emails to test, in the daemon's own words; empty when nothing is. Before
 * the settings say the daemon's largest number only a whole number from 1 is asked for; the daemon still
 * refuses a larger one, and the tester shows its sentence.
 */
export function limitProblem(n: number | null) {
  const max = settings.value.limits.test_max;
  if (n !== null && Number.isInteger(n) && n >= 1 && (!max || n <= max)) return '';
  return max ? `The limit must be between 1 and ${max}.` : 'Give a whole number of emails, 1 or more.';
}
