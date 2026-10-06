import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { StatsUsage } from '../lib/api/usage';
import { usage } from '../lib/state/usage.svelte';
import Usage from './Usage.svelte';

let reply: [status: number, body: unknown];

const stats = (over: Partial<StatsUsage> = {}): StatsUsage => ({
  range: 'month',
  since: 1788739200,
  processed: 0,
  emails: 0,
  calls: 0,
  cost_usd: 0,
  days: [
    { day: '2026-10-05', models: [] },
    { day: '2026-10-06', models: [] },
  ],
  by_rule: [],
  by_model: [],
  without_model: 0,
  ...over,
});

beforeEach(() => {
  // The state lives in module scope; each test starts it over.
  Object.assign(usage, { status: 'loading', error: '', summary: null });
  reply = [200, stats()];
  vi.stubGlobal(
    'fetch',
    vi.fn(async () => new Response(JSON.stringify(reply[1]), { status: reply[0] })),
  );
});
afterEach(() => vi.unstubAllGlobals());

it('a failed load shows an alert, and Retry fetches again', async () => {
  reply = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Usage);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  reply = [200, stats()];
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Emails sorted')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('a month with nothing sorted says so', async () => {
  render(Usage);
  expect(await screen.findByText('No emails sorted yet.')).toBeTruthy();
  expect(screen.getByText('$0.00')).toBeTruthy();
});

it('shows the totals, the days, each rule and each model', async () => {
  reply = [
    200,
    stats({
      processed: 200,
      emails: 200,
      without_model: 150,
      calls: 53,
      cost_usd: 1.75,
      days: [
        { day: '2026-10-05', models: [] },
        { day: '2026-10-06', models: [{ provider: 'openrouter', model: 'jev', calls: 40, cost_usd: 0.5 }] },
      ],
      by_rule: [
        { rule_id: 5, rule_name: 'Newsletters', emails: 120, calls: 40, cost_usd: 0.5 },
        { rule_id: 6, rule_name: 'Receipts', emails: 30, calls: 0, cost_usd: 0 },
      ],
      by_model: [
        { provider: 'openrouter', model: 'jev', purpose: 'decide', calls: 40, tokens_in: 9000, tokens_out: 1000, cost_usd: 0.5 },
        { provider: 'anthropic', model: 'haiku', purpose: 'escalate', calls: 10, tokens_in: 0, tokens_out: 0, cost_usd: 1 },
        { provider: 'anthropic', model: 'haiku', purpose: 'compose', calls: 3, tokens_in: 0, tokens_out: 0, cost_usd: 0.25 },
      ],
    }),
  ];
  render(Usage);

  // The tiles: calls and cost are the ledger's totals, composing included.
  expect(await screen.findByText('200')).toBeTruthy();
  expect(screen.getByText('75%')).toBeTruthy();
  expect(screen.getByText('53')).toBeTruthy();
  expect(screen.getByText('decision model 40 · fallback 10')).toBeTruthy();
  expect(screen.getByText('$1.75')).toBeTruthy();

  expect(screen.getByRole('region', { name: 'Model calls by day' }).textContent).toContain('jev 40');

  const byRule = screen.getByRole('region', { name: 'By rule' });
  expect(byRule.textContent).toContain('Newsletters');
  expect(byRule.textContent).toContain('Free');
  expect(byRule.textContent).toContain('Composing and testing rules are not counted per rule.');

  const byModel = screen.getByRole('region', { name: 'By model' });
  expect(byModel.textContent).toContain('jev (decision model)');
  expect(byModel.textContent).toContain('40 calls · 10k tokens');
  expect(byModel.textContent).toContain('haiku (rule composer)');
  expect(byModel.textContent).toContain('150 emails');
});
