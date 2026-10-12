import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Limits } from '../lib/api/limits';
import type { StatsUsage } from '../lib/api/usage';
import { limits } from '../lib/state/limits.svelte';
import { usage } from '../lib/state/usage.svelte';
import Usage from './Usage.svelte';

let reply: [status: number, body: unknown];
/** GET /api/limits: self-host's, with no limits, unless a test sets a plan. */
let plan: Limits;
const selfHost: Limits = { mailboxes: null, ai_emails: null, upgrade_url: null };

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
  limits.value = null;
  reply = [200, stats()];
  plan = selfHost;
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string) => (url === '/api/limits' ? new Response(JSON.stringify(plan)) : new Response(JSON.stringify(reply[1]), { status: reply[0] }))),
  );
});
afterEach(() => vi.unstubAllGlobals());

it('a failed load shows an alert, and Retry fetches again', async () => {
  reply = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Usage);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  reply = [200, stats()];
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Emails acted on')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('a month with nothing decided says so', async () => {
  render(Usage);
  expect(await screen.findByText('No emails decided yet.')).toBeTruthy();
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
        { day: '2026-10-06', models: [{ provider: 'openrouter', model: 'jev', purpose: 'decide', calls: 40, cost_usd: 0.5 }] },
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

describe('the plan', () => {
  // 1 Nov 2026, 00:00 UTC: when October's count ends, as the browser's locale writes it.
  const nov1 = Date.UTC(2026, 10, 1) / 1000;
  const nov = new Date(nov1 * 1000).toLocaleDateString([], { day: 'numeric', month: 'long', timeZone: 'UTC' });
  const ai = (over: Partial<NonNullable<Limits['ai_emails']>> = {}) => ({ used: 1200, limit: 3000, month: '2026-10', resets_at: nov1, counted: true, paused: false, ...over });
  const meter = (name: string) => {
    const m = screen.getByRole('meter', { name });
    return [m.getAttribute('aria-valuenow'), m.getAttribute('aria-valuemax'), m.getAttribute('aria-valuetext')];
  };

  it('is not shown on self-host', async () => {
    render(Usage);
    await screen.findByText('Emails acted on');
    expect(screen.queryByRole('region', { name: 'Your plan' })).toBeNull();
  });

  it('meters the mailboxes and the month’s AI-sorted emails against the plan', async () => {
    plan = { mailboxes: { used: 2, limit: 3, refusal: null }, ai_emails: ai(), upgrade_url: null };
    render(Usage);
    const region = await screen.findByRole('region', { name: 'Your plan' });
    expect(meter('Mailboxes')).toEqual(['2', '3', '2 of 3']);
    expect(meter('AI-sorted emails this month')).toEqual(['1200', '3000', (1200).toLocaleString() + ' of ' + (3000).toLocaleString()]);
    expect(region.textContent).toContain(`Counts start again on ${nov}.`);
    expect(within(region).queryByRole('status')).toBeNull();
    expect(within(region).queryByRole('link')).toBeNull(); // no upgrade without a link from the hosted service
  });

  it('shows no limit while the tenant’s own keys decide', async () => {
    plan = { mailboxes: null, ai_emails: ai({ counted: false }), upgrade_url: null };
    render(Usage);
    await screen.findByRole('region', { name: 'Your plan' });
    expect(meter('AI-sorted emails this month')).toEqual(['0', '0', (1200).toLocaleString() + ' · no limit with your own keys']);
    expect(screen.queryByRole('meter', { name: 'Mailboxes' })).toBeNull();
  });

  it.each([
    ['with an upgrade', 'https://mailrules.app/plan', ['Add your own model key', 'upgrade']],
    ['without one', null, ['Add your own model key']],
  ])('used up, says mail waits and offers the own key, %s', async (_name, upgrade, links) => {
    plan = { mailboxes: null, ai_emails: ai({ used: 3000, paused: true }), upgrade_url: upgrade };
    render(Usage);
    const banner = await screen.findByRole('status');
    const status = within(banner);
    expect(banner.textContent).toContain(`Until ${nov}, email that needs AI waits in Needs review`);
    expect(status.getAllByRole('link').map((a) => a.textContent)).toEqual(links);
    expect(status.getByRole('link', { name: 'Add your own model key' }).getAttribute('href')).toBe('#/settings');
    if (upgrade) expect(status.getByRole('link', { name: 'upgrade' }).getAttribute('href')).toBe(upgrade);
  });
});
