import { subscribe } from '../api/events';
import * as planApi from '../api/plan';
import { load as loadLimits } from './limits.svelte';
import { load as loadSettings } from './settings.svelte';

/**
 * The tenant's plan, in a build that sells plans; null elsewhere and until loaded. `waiting` is set
 * once a checkout completed or a change was asked, and cleared when the billing service's answer
 * lands (`plan.changed`): the plan screen says it waits until then.
 */
export const plan = $state<{ value: planApi.PlanInfo | null; error: string; waiting: boolean }>({ value: null, error: '', waiting: false });

export async function load() {
  try {
    plan.value = await planApi.get();
    plan.error = '';
  } catch (e) {
    plan.error = (e instanceof Error && e.message) || 'The daemon did not answer.';
  }
}

/** The plan has ended: nothing is sorted and the daemon refuses changes (403 plan_ended). */
export const readOnly = () => plan.value?.read_only === true;

// The plan changed: what it allows (features in the settings, the limits) changed with it.
subscribe('plan.changed', () => {
  plan.waiting = false;
  load();
  loadSettings();
  loadLimits();
});
