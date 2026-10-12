import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dispatch } from '../lib/api/events';
import type { PlanInfo } from '../lib/api/plan';
import { fullDay } from '../lib/format';
import { error, serve } from '../lib/state/activity.fixtures';
import { limits } from '../lib/state/limits.svelte';
import { plan } from '../lib/state/plan.svelte';
import Plan from './Plan.svelte';

const day = 86400;
const trialEnd = 1792000000;

const info = (over: Partial<PlanInfo> = {}): PlanInfo => ({
  plan: 'trial',
  interval: null,
  status: null,
  period_end: null,
  trial_ends_at: trialEnd,
  trial_days_left: 5,
  ended_at: null,
  secrets_deleted_at: null,
  removed_at: null,
  read_only: false,
  subscribed: false,
  note: 'Billed by the billing service, taxes included at checkout.',
  plans: [
    { id: 'personal', name: 'Personal', prices: { month: '$5 a month', year: '$50 a year' }, includes: ['3 mailboxes', '3,000 AI-sorted emails a month'], mailboxes: 3, ai_emails: 3000, features: [] },
    { id: 'pro', name: 'Pro', prices: { month: '$10 a month', year: '$100 a year' }, includes: ['10 mailboxes', 'Suggest from my mail'], mailboxes: 10, ai_emails: 10000, features: ['suggest'] },
  ],
  ...over,
});
const pro = (over: Partial<PlanInfo> = {}) => info({ plan: 'pro', interval: 'month', status: 'active', period_end: trialEnd + 30 * day, trial_days_left: null, subscribed: true, ...over });
const selfHostLimits = { mailboxes: null, ai_emails: null, first_cleanup: null, upgrade_url: null };

/** The daemon: GET /api/plan answers `current`; any other call `other(call, body)`. */
function daemon(current: () => PlanInfo, other: (call: string, body: unknown) => [number, unknown?] = () => [404, error('not_found', 'Not found.')]) {
  return serve((call, body) => {
    if (call === 'GET /api/plan') return [200, current()];
    if (call === 'GET /api/limits') return [200, selfHostLimits];
    return other(call, body);
  });
}
const card = (name: string) => screen.findByRole('article', { name });

beforeEach(() => {
  Object.assign(plan, { value: null, error: '', waiting: false });
  limits.value = null;
});
afterEach(() => {
  vi.unstubAllGlobals();
  document.head.querySelectorAll('script').forEach((s) => s.remove());
  delete window.mailrulesCheckout;
});

it('in the trial, says how many days are left and offers each plan to choose, priced by the interval', async () => {
  daemon(() => info());
  render(Plan);
  const region = await screen.findByRole('region', { name: 'Your plan' });
  expect(screen.getByText(/You're on the free trial\./)).toBeTruthy();
  expect(region.textContent).toContain(`5 days left in your trial; it ends ${fullDay(trialEnd)}.`);
  expect(region.textContent).toContain('Billed by the billing service, taxes included at checkout.');
  expect(within(region).queryByRole('button', { name: 'Manage billing' })).toBeNull();

  const personal = await card('Personal');
  expect(personal.textContent).toContain('$5 a month');
  expect(personal.textContent).toContain('3,000 AI-sorted emails a month');
  expect(within(personal).getByRole('button', { name: 'Choose Personal' })).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Yearly' }));
  expect(personal.textContent).toContain('$50 a year');
  expect((await card('Pro')).textContent).toContain('$100 a year');
});

it('marks the current plan at its interval only', async () => {
  daemon(() => pro());
  render(Plan);
  const current = within(await card('Pro')).getByRole('button', { name: 'Current plan' }) as HTMLButtonElement;
  expect(current.disabled).toBe(true);
  expect(within(await card('Personal')).getByRole('button', { name: 'Switch to Personal' })).toBeTruthy();

  await fireEvent.click(screen.getByRole('button', { name: 'Yearly' }));
  expect(within(await card('Pro')).getByRole('button', { name: 'Switch to Pro' })).toBeTruthy();
});

describe('choosing a plan without a subscription', () => {
  const checkout = { transaction: 'txn_1', customer: { email: 'ana@example.com' } };

  it('asks for checkout, loads its script once and opens it; a completed checkout waits for the billing service', async () => {
    const calls = daemon(() => info(), (call) => (call === 'POST /api/plan/checkout' ? [200, { checkout, script: '/api/m/billing/checkout.js' }] : [404]));
    const open = vi.fn();
    render(Plan);

    // The script loads: the module's code puts the checkout on the window.
    await fireEvent.click(within(await card('Pro')).getByRole('button', { name: 'Choose Pro' }));
    await waitFor(() => expect(document.head.querySelectorAll('script')).toHaveLength(1));
    const script = document.head.querySelector('script')!;
    expect(script.getAttribute('src')).toBe('/api/m/billing/checkout.js');
    window.mailrulesCheckout = { open };
    script.dispatchEvent(new Event('load'));
    await waitFor(() => expect(open).toHaveBeenCalledTimes(1));
    expect(open.mock.calls[0][0]).toEqual(checkout);
    expect(calls.find((c) => c.call === 'POST /api/plan/checkout')?.body).toEqual({ plan: 'pro', interval: 'month' });

    // Closed without paying: nothing to wait for. Asked again, the loaded script is used.
    open.mock.calls[0][1]('closed');
    await fireEvent.click(within(await card('Personal')).getByRole('button', { name: 'Choose Personal' }));
    await waitFor(() => expect(open).toHaveBeenCalledTimes(2));
    expect(document.head.querySelectorAll('script')).toHaveLength(1);
    expect(screen.queryByText('Waiting for the billing service to confirm…')).toBeNull();

    open.mock.calls[1][1]('completed');
    expect(await screen.findByText('Waiting for the billing service to confirm…')).toBeTruthy();
  });

  it('shows a refusal at the card', async () => {
    daemon(() => info(), () => [409, error('downgrade_refused', 'Personal allows 3 mailboxes and you have 4. Remove Work or Old to choose it.')]);
    render(Plan);
    const personal = await card('Personal');
    await fireEvent.click(within(personal).getByRole('button', { name: 'Choose Personal' }));
    expect((await within(personal).findByRole('alert')).textContent).toBe('Personal allows 3 mailboxes and you have 4. Remove Work or Old to choose it.');
    expect(within(await card('Pro')).queryByRole('alert')).toBeNull();
    expect(document.head.querySelector('script')).toBeNull();
  });
});

describe('a subscribed tenant', () => {
  it('switches with a change, waits for the billing service, and refetches the plan, settings and limits once it lands', async () => {
    let current = pro();
    const calls = daemon(
      () => current,
      (call) => (call === 'POST /api/plan/change' ? [202, current] : call === 'GET /api/settings' ? [500, error('internal', 'x')] : [404]),
    );
    render(Plan);
    await fireEvent.click(within(await card('Personal')).getByRole('button', { name: 'Switch to Personal' }));
    expect(await screen.findByText('Waiting for the billing service to confirm…')).toBeTruthy();
    expect(calls.find((c) => c.call === 'POST /api/plan/change')?.body).toEqual({ plan: 'personal', interval: 'month' });
    expect(document.head.querySelector('script')).toBeNull();

    current = pro({ plan: 'personal' });
    const before = calls.length;
    dispatch('plan.changed', {});
    expect(await within(await card('Personal')).findByRole('button', { name: 'Current plan' })).toBeTruthy();
    expect(screen.queryByText('Waiting for the billing service to confirm…')).toBeNull();
    expect(new Set(calls.slice(before).map((c) => c.call))).toEqual(new Set(['GET /api/plan', 'GET /api/settings', 'GET /api/limits']));
  });

  it('shows a refused downgrade at the card', async () => {
    daemon(() => pro(), () => [409, error('downgrade_refused', 'Remove 2 mailboxes first: Work, Old.')]);
    render(Plan);
    const personal = await card('Personal');
    await fireEvent.click(within(personal).getByRole('button', { name: 'Switch to Personal' }));
    expect((await within(personal).findByRole('alert')).textContent).toBe('Remove 2 mailboxes first: Work, Old.');
    expect(screen.queryByText('Waiting for the billing service to confirm…')).toBeNull();
  });

  it('opens the billing service’s portal from Manage billing', async () => {
    vi.stubGlobal('location', { ...window.location, assign: vi.fn() });
    daemon(() => pro(), (call) => (call === 'GET /api/plan/portal' ? [200, { url: 'https://billing.example/portal/abc' }] : [404]));
    render(Plan);
    await fireEvent.click(await screen.findByRole('button', { name: 'Manage billing' }));
    await waitFor(() => expect(window.location.assign).toHaveBeenCalledWith('https://billing.example/portal/abc'));
  });

  it.each([
    ['renews', pro(), `Renews ${fullDay(trialEnd + 30 * day)}.`],
    ['ends when cancelled', pro({ status: 'cancels_at_period_end' }), `Ends ${fullDay(trialEnd + 30 * day)}; it won't renew.`],
    ['keeps running while a payment is retried', pro({ status: 'past_due' }), 'Payment failed; the billing service is retrying it, and your plan keeps running.'],
  ])('%s', async (_name, current, line) => {
    daemon(() => current);
    render(Plan);
    expect((await screen.findByRole('region', { name: 'Your plan' })).textContent).toContain(line);
  });
});
