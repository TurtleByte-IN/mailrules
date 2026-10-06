// DEMO: GET /api/stats/summary?range= is not in api/openapi.yaml yet (backend M9). The range
// values are cut off in docs/backend-plan.md, so get() takes none and returns this month.
import { fake } from './demo';

/** usage_daily in docs/backend-plan.md → Data model, summed over the range. */
export interface ModelUsage {
  model: string;
  purpose: 'decide' | 'escalate' | 'compose' | 'test';
  calls: number;
  tokensIn: number;
  tokensOut: number;
  costUsd: number;
}

export interface DayUsage {
  /** YYYY-MM-DD, UTC. */
  day: string;
  decide: number;
  escalate: number;
}

export interface RuleUsage {
  ruleId: string;
  emails: number;
  calls: number;
  costUsd: number;
}

export interface Summary {
  emails: number;
  /** Decided by conditions and sender rules, with no model call. */
  freeEmails: number;
  budgetUsd: number;
  /** The last 14 days, oldest first. */
  days: DayUsage[];
  models: ModelUsage[];
  rules: RuleUsage[];
}

const decide = [72, 64, 81, 58, 33, 29, 77, 85, 69, 74, 61, 38, 31, 70];
const escalate = [8, 6, 11, 5, 2, 3, 9, 7, 6, 10, 5, 3, 2, 7];
const utcDay = (daysAgo: number) => new Date(Date.now() - daysAgo * 86400_000).toISOString().slice(0, 10);

// The prototype's figures: nine emails per hit, and a model call for 80% of what an AI rule sorts.
const rule = (ruleId: string, hits: number, ai: boolean): RuleUsage => {
  const calls = ai ? Math.round(hits * 9 * 0.8) : 0;
  return { ruleId, emails: hits * 9, calls, costUsd: calls * 0.00011 };
};

const summary = (): Summary => ({
  emails: 3912,
  freeEmails: 2778,
  budgetUsd: 5,
  days: decide.map((d, i) => ({ day: utcDay(13 - i), decide: d, escalate: escalate[i] })),
  models: [
    { model: 'Jev', purpose: 'decide', calls: 1021, tokensIn: 380_000, tokensOut: 32_000, costUsd: 0.02 },
    { model: 'Claude Haiku 4.5', purpose: 'escalate', calls: 113, tokensIn: 180_000, tokensOut: 14_000, costUsd: 0.21 },
    { model: 'Claude Haiku 4.5', purpose: 'compose', calls: 14, tokensIn: 0, tokensOut: 0, costUsd: 0.04 },
  ],
  rules: [rule('r1', 38, false), rule('r2', 12, false), rule('r3', 9, true), rule('r4', 3, true), rule('r5', 51, true), rule('r6', 17, true)],
});

export const get = () => fake(summary());
