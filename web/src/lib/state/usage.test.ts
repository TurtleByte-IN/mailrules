import { expect, it } from 'vitest';
import * as usageApi from '../api/usage';
import { totals } from './usage.svelte';

const model = (purpose: usageApi.ModelUsage['purpose'], calls: number, costUsd: number): usageApi.ModelUsage => ({
  model: 'm',
  purpose,
  calls,
  tokensIn: 0,
  tokensOut: 0,
  costUsd,
});
const summary = (emails: number, freeEmails: number, models: usageApi.ModelUsage[]): usageApi.Summary => ({
  emails,
  freeEmails,
  budgetUsd: 5,
  days: [],
  models,
  rules: [],
});

it.each([
  ['nothing sorted yet', summary(0, 0, []), { emails: 0, freePct: 0, decide: 0, escalate: 0, calls: 0, costUsd: 0 }],
  [
    'composer calls cost money but are not sorting calls',
    summary(200, 150, [model('decide', 40, 0.5), model('escalate', 10, 1), model('compose', 3, 0.25)]),
    { emails: 200, freePct: 75, decide: 40, escalate: 10, calls: 50, costUsd: 1.75 },
  ],
  [
    'one purpose spread over two models is summed',
    summary(3, 1, [model('decide', 1, 0), model('decide', 2, 0)]),
    { emails: 3, freePct: 33, decide: 3, escalate: 0, calls: 3, costUsd: 0 },
  ],
])('totals: %s', (_name, s, want) => expect(totals(s)).toEqual(want));

it('the demo summary gives the prototype tiles', async () => {
  const t = totals(await usageApi.get());
  expect([t.emails, t.freePct, t.calls, t.decide, t.escalate]).toEqual([3912, 71, 1134, 1021, 113]);
  expect(t.costUsd).toBeCloseTo(0.27);
});
