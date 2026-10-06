import { subscribe } from '../api/events';
import * as usageApi from '../api/usage';

export const usage = $state<{
  status: 'loading' | 'ready' | 'error';
  error: string;
  summary: usageApi.StatsUsage | null;
}>({ status: 'loading', error: '', summary: null });

export async function load() {
  try {
    usage.summary = await usageApi.get();
    usage.status = 'ready';
  } catch (e) {
    usage.status = 'error';
    usage.error = (e as Error).message;
  }
}

// Only a screen that already shows numbers refetches them.
subscribe('usage.updated', () => {
  if (usage.status === 'ready') load();
});

const sum = (xs: number[]) => xs.reduce((a, x) => a + x, 0);

/** The four tiles. Calls and cost are the API's totals; the split by purpose comes from by_model. */
export function totals(s: usageApi.StatsUsage) {
  const calls = (purpose: usageApi.ModelUsage['purpose']) => sum(s.by_model.filter((m) => m.purpose === purpose).map((m) => m.calls));
  return {
    emails: s.emails,
    freePct: s.processed ? Math.round((s.without_model / s.processed) * 100) : 0,
    decide: calls('decide'),
    escalate: calls('escalate'),
    calls: s.calls,
    costUsd: s.cost_usd,
  };
}
