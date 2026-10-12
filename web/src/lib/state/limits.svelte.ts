import { subscribe } from '../api/events';
import * as limitsApi from '../api/limits';

/**
 * The tenant's use against the hosted service's limits: what Usage meters, what Needs review warns of and what
 * Mailboxes refuses. null until loaded, and while it cannot be: the screens then say nothing of limits, and the
 * daemon still refuses what is over them.
 */
export const limits = $state<{ value: limitsApi.Limits | null }>({ value: null });

export async function load() {
  try {
    limits.value = await limitsApi.get();
  } catch {
    // Nothing to show; the last answer, if any, stays.
  }
}

// A model decision may have used the last of the month's allowance.
subscribe('usage.updated', () => {
  if (limits.value) load();
});
