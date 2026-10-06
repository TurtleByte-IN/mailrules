import * as usageApi from '../api/usage';

export const usage = $state<{ summary: usageApi.Summary | null }>({ summary: null });

export async function load() {
  usage.summary = await usageApi.get();
}

const sum = (xs: number[]) => xs.reduce((a, x) => a + x, 0);

/** The four tiles. Model calls count sorting only; the rule composer's calls still cost money. */
export function totals(s: usageApi.Summary) {
  const calls = (purpose: usageApi.ModelUsage['purpose']) => sum(s.models.filter((m) => m.purpose === purpose).map((m) => m.calls));
  const decide = calls('decide');
  const escalate = calls('escalate');
  return {
    emails: s.emails,
    freePct: s.emails ? Math.round((s.freeEmails / s.emails) * 100) : 0,
    decide,
    escalate,
    calls: decide + escalate,
    costUsd: sum(s.models.map((m) => m.costUsd)),
  };
}
