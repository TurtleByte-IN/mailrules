import { render, screen, within } from '@testing-library/svelte';
import { beforeEach, expect, it } from 'vitest';
import type { Role } from '../api/members';
import type { PlanInfo } from '../api/plan';
import { fullDay } from '../format';
import { auth } from '../state/auth.svelte';
import { plan } from '../state/plan.svelte';
import { settings } from '../state/settings.svelte';
import PlanBanner from './PlanBanner.svelte';

const day = 86400;
const now = Math.floor(Date.now() / 1000);
const trialEnd = now - 2 * day;

const info = (over: Partial<PlanInfo> = {}): PlanInfo => ({
  plan: 'trial',
  interval: null,
  status: null,
  period_end: null,
  seats: null,
  trial_ends_at: trialEnd,
  trial_days_left: 5,
  ended_at: null,
  secrets_deleted_at: null,
  removed_at: null,
  read_only: false,
  subscribed: false,
  note: null,
  plans: [],
  ...over,
});
const ended = (over: Partial<PlanInfo> = {}) =>
  info({ plan: 'ended', trial_days_left: null, ended_at: trialEnd, secrets_deleted_at: trialEnd + 30 * day, removed_at: trialEnd + 90 * day, read_only: true, ...over });

const billing = (on: boolean) => (settings.value = { ...settings.value, features: { ...settings.value.features, billing: on } });

const as = (role: Role) => (auth.user = { id: 1, email: 'ana@example.com', role });

beforeEach(() => {
  billing(true);
  as('owner');
  plan.value = null;
});

it.each([
  ['a trial', ended(), 'Your trial has ended.'],
  ['a plan', ended({ ended_at: trialEnd + 40 * day, secrets_deleted_at: trialEnd + 70 * day, removed_at: trialEnd + 130 * day }), 'Your plan has ended.'],
])('once %s has ended, says sorting stopped, when what is deleted, and offers to subscribe', (_name, current, title) => {
  plan.value = current;
  render(PlanBanner);
  const banner = screen.getByRole('status');
  expect(banner.textContent).toContain(title);
  expect(banner.textContent).toContain('MailRules has stopped sorting. Activity, rules and settings stay readable.');
  expect(banner.textContent).toContain(`Mailbox passwords are deleted on ${fullDay(current.secrets_deleted_at!)} and everything else on ${fullDay(current.removed_at!)}.`);
  expect(within(banner).getByRole('link', { name: 'Subscribe' }).getAttribute('href')).toBe('#/plan');
});

it('says the passwords were deleted once that day has passed', () => {
  plan.value = ended({ ended_at: now - 40 * day, secrets_deleted_at: now - 10 * day, removed_at: now + 50 * day });
  render(PlanBanner);
  expect(screen.getByRole('status').textContent).toContain(`Mailbox passwords were deleted on ${fullDay(now - 10 * day)}`);
});

it.each([
  ['a running trial', info()],
  ['a running plan', info({ plan: 'pro', trial_days_left: null, subscribed: true })],
])('says nothing during %s', (_name, current) => {
  plan.value = current;
  const { container } = render(PlanBanner);
  expect(container.textContent?.trim()).toBe('');
});

it('says nothing in a build that sells no plans', () => {
  billing(false);
  plan.value = ended();
  const { container } = render(PlanBanner);
  expect(container.textContent?.trim()).toBe('');
});

it('in the last days of the trial, offers a plan', () => {
  plan.value = info({ trial_days_left: 2 });
  render(PlanBanner);
  expect(screen.getByText(/Your trial ends in 2 days\./)).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Choose a plan' }).getAttribute('href')).toBe('#/plan');
  expect(screen.queryByRole('status')).toBeNull();
});

it.each(['admin', 'member'] as const)('tells an %s the plan ended and who can subscribe, with no link to buy', (role) => {
  as(role);
  plan.value = ended();
  render(PlanBanner);
  const banner = screen.getByRole('status');
  expect(banner.textContent).toContain('Your trial has ended.');
  expect(banner.textContent).toContain("Ask the team's owner to subscribe.");
  expect(within(banner).queryByRole('link')).toBeNull();
});

it('tells a member the trial is ending and who can subscribe', () => {
  as('member');
  plan.value = info({ trial_days_left: 1 });
  render(PlanBanner);
  expect(screen.getByText(/Your trial ends in 1 day\. Ask the team's owner to subscribe\./)).toBeTruthy();
  expect(screen.queryByRole('link')).toBeNull();
});
