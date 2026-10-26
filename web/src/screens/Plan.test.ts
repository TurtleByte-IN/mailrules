import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { dispatch } from '../lib/api/events';
import type { PlanInfo } from '../lib/api/plan';
import { fullDay } from '../lib/format';
import { error, serve } from '../lib/state/activity.fixtures';
import { auth } from '../lib/state/auth.svelte';
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
  seats: null,
  seats_used: null,
  company_sign_in: null,
  trial_ends_at: trialEnd,
  trial_days_left: 5,
  ended_at: null,
  secrets_deleted_at: null,
  removed_at: null,
  read_only: false,
  subscribed: false,
  note: 'Billed by the billing service, taxes included at checkout.',
  plans: [
    { id: 'personal', name: 'Personal', prices: { month: '$5 a month', year: '$50 a year' }, includes: ['3 mailboxes', '3,000 AI-sorted emails a month'], mailboxes: 3, ai_emails: 3000, features: [], per_seat: false, min_seats: null, company_sign_in: null },
    { id: 'pro', name: 'Pro', prices: { month: '$10 a month', year: '$100 a year' }, includes: ['10 mailboxes', 'Suggest from my mail'], mailboxes: 10, ai_emails: 10000, features: ['suggest'], per_seat: false, min_seats: null, company_sign_in: null },
  ],
  ...over,
});
const pro = (over: Partial<PlanInfo> = {}) => info({ plan: 'pro', interval: 'month', status: 'active', period_end: trialEnd + 30 * day, trial_days_left: null, subscribed: true, ...over });
const teamTerms: PlanInfo['plans'][number] = {
  id: 'team', name: 'Team', prices: { month: '$8 a month', year: '$80 a year' }, includes: ['Shared rules', 'Audit log'], mailboxes: 10, ai_emails: 10000, features: ['team'], per_seat: true, min_seats: 2, company_sign_in: null,
};
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

describe('the Team plan, sold by the seat', () => {
  afterEach(() => {
    auth.members = 1;
  });

  it('prices it per seat, asks for enough seats for the team, and sends the count with checkout', async () => {
    auth.members = 3;
    const calls = daemon(() => info({ plans: [...info().plans, teamTerms] }), (call) => (call === 'POST /api/plan/checkout' ? [200, { checkout: {}, script: '/api/m/billing/checkout.js' }] : [404]));
    render(Plan);
    const team = await card('Team');
    expect(team.textContent).toContain('Per seat, from 2 seats');
    const seats = within(team).getByLabelText('Seats') as HTMLInputElement;
    expect(seats.value).toBe('3');

    await fireEvent.input(seats, { target: { value: '5' } });
    await fireEvent.click(within(team).getByRole('button', { name: 'Choose Team' }));
    await waitFor(() => expect(calls.find((c) => c.call === 'POST /api/plan/checkout')?.body).toEqual({ plan: 'team', interval: 'month', seats: 5 }));
  });

  it('never asks for fewer seats than the plan sells', async () => {
    daemon(() => info({ plans: [teamTerms] }));
    render(Plan);
    expect((within(await card('Team')).getByLabelText('Seats') as HTMLInputElement).value).toBe('2');
  });

  it('shows the seats a subscribed team has, and changes only the count with a change', async () => {
    auth.members = 2;
    const current = pro({ plan: 'team', seats: 5, seats_used: 4, plans: [...info().plans, teamTerms] });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/change' ? [202, current] : [404]));
    render(Plan);
    expect((await screen.findByRole('region', { name: 'Your plan' })).textContent).toContain('Your plan has 5 seats; 4 are taken by members and invitations waiting.');
    const team = await card('Team');
    expect(within(team).getByRole('button', { name: 'Current plan' })).toBeTruthy();

    await fireEvent.input(within(team).getByLabelText('Seats'), { target: { value: '8' } });
    await fireEvent.click(within(team).getByRole('button', { name: 'Change seats' }));
    expect(await screen.findByText('Waiting for the billing service to confirm…')).toBeTruthy();
    expect(calls.find((c) => c.call === 'POST /api/plan/change')?.body).toEqual({ plan: 'team', interval: 'month', seats: 8 });
  });

  it('sends no seats for a plan not sold by the seat', async () => {
    const current = pro({ plan: 'team', seats: 5, plans: [...info().plans, teamTerms] });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/change' ? [202, current] : [404]));
    render(Plan);
    await fireEvent.click(within(await card('Pro')).getByRole('button', { name: 'Switch to Pro' }));
    await waitFor(() => expect(calls.find((c) => c.call === 'POST /api/plan/change')?.body).toEqual({ plan: 'pro', interval: 'month' }));
  });
});

describe('company sign-in on the Team plan', () => {
  const ssoTerms = { ...teamTerms, company_sign_in: { included_seats: 10, prices: { month: '$180 a month', year: '$1,800 a year' } } };
  const team = (seats: number, cs: { allowed: boolean; add_on: boolean }) =>
    pro({ plan: 'team', seats, seats_used: 3, plans: [...info().plans, ssoTerms], company_sign_in: { ...cs, included_seats: 10, ends_at: null } });
  const section = async () => within(await screen.findByRole('group', { name: 'Company sign-in' }));

  it('offers the add-on below 10 seats, at the plan’s interval, and waits for the billing service', async () => {
    const current = team(3, { allowed: false, add_on: false });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/company-sign-in' ? [202, current] : [404]));
    render(Plan);
    const s = await section();
    expect(s.getByText(/Included from 10 seats, or add it now for \$180 a month\./)).toBeTruthy();
    expect((await card('Team')).textContent).toContain('Company sign-in from 10 seats, or $180 a month below that');
    await fireEvent.click(s.getByRole('button', { name: 'Add company sign-in' }));
    expect(await screen.findByText('Waiting for the billing service to confirm…')).toBeTruthy();
    expect(calls.find((c) => c.call === 'POST /api/plan/company-sign-in')?.body).toEqual({ add_on: true });
  });

  it('asks before removing an add-on the team needs, and says it ends', async () => {
    const current = team(3, { allowed: true, add_on: true });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/company-sign-in' ? [202, current] : [404]));
    render(Plan);
    const s = await section();
    await fireEvent.click(s.getByRole('button', { name: 'Remove the add-on' }));
    const ask = s.getByRole('alertdialog');
    expect(ask.textContent).toContain('Company sign-in ends for your team once the billing service confirms');
    expect(calls.some((c) => c.call === 'POST /api/plan/company-sign-in')).toBe(false);
    await fireEvent.click(within(ask).getByRole('button', { name: 'Yes, remove it' }));
    await waitFor(() => expect(calls.find((c) => c.call === 'POST /api/plan/company-sign-in')?.body).toEqual({ add_on: false }));
  });

  it('at 10 seats, includes it and warns that fewer seats end it', async () => {
    daemon(() => team(10, { allowed: true, add_on: false }));
    render(Plan);
    const s = await section();
    expect(s.getByText(/Included with your 10 seats\./)).toBeTruthy();
    expect(s.getByText(/Under 10 seats it needs the add-on \(\$180 a month\)/)).toBeTruthy();
    expect(s.queryByRole('button', { name: 'Add company sign-in' })).toBeNull();
  });

  it.each([
    ['fewer seats', 'Team', 'Change seats', '9', 'under 10 seats it needs the add-on', { plan: 'team', interval: 'month', seats: 9 }],
    ['another plan', 'Pro', 'Switch to Pro', '', 'it is part of the Team plan only', { plan: 'pro', interval: 'month' }],
  ])('warns before a change to %s ends it, and changes only once the Owner accepts', async (_name, name, button, seats, why, body) => {
    const current = team(10, { allowed: true, add_on: false });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/change' ? [202, current] : [404]));
    render(Plan);
    const c = await card(name);
    if (seats) await fireEvent.input(within(c).getByLabelText('Seats'), { target: { value: seats } });
    expect(c.textContent).toContain(`Company sign-in ends with this change: ${why}.`);
    await fireEvent.click(within(c).getByRole('button', { name: button }));
    expect(calls.some((x) => x.call === 'POST /api/plan/change')).toBe(false);
    await fireEvent.click(within(within(c).getByRole('alertdialog')).getByRole('button', { name: /^Yes, / }));
    await waitFor(() => expect(calls.find((x) => x.call === 'POST /api/plan/change')?.body).toEqual(body));
  });

  it('does not warn when the add-on keeps it', async () => {
    const current = team(10, { allowed: true, add_on: true });
    const calls = daemon(() => current, (call) => (call === 'POST /api/plan/change' ? [202, current] : [404]));
    render(Plan);
    const c = await card('Team');
    await fireEvent.input(within(c).getByLabelText('Seats'), { target: { value: '4' } });
    expect(c.textContent).not.toContain('Company sign-in ends');
    await fireEvent.click(within(c).getByRole('button', { name: 'Change seats' }));
    await waitFor(() => expect(calls.find((x) => x.call === 'POST /api/plan/change')?.body).toEqual({ plan: 'team', interval: 'month', seats: 4 }));
  });
});
