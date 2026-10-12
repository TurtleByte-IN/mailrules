import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Limits } from '../lib/api/limits';
import { error, inReview, item, serve } from '../lib/state/activity.fixtures';
import { limits } from '../lib/state/limits.svelte';
import { review } from '../lib/state/review.svelte';
import Review from './Review.svelte';

// The state lives in module scope and a mounted screen cannot be given fresh modules, so each test starts it over.
beforeEach(() => {
  Object.assign(review, { list: [], next: null, total: 0, loaded: false, error: '' });
  limits.value = null;
});
afterEach(() => vi.unstubAllGlobals());

const queue = (ids: number[]) => ({ items: ids.map(inReview), next_cursor: null, total: ids.length });

it('a failed load is an alert, and Retry asks again', async () => {
  const calls = serve(() => [500, error('internal', 'Something went wrong.')]);
  render(Review);
  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toContain('Something went wrong.');
  expect(screen.queryByText('All clear')).toBeNull();

  await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
  expect(calls.map((c) => c.call).filter((c) => c !== 'GET /api/limits')).toEqual(['GET /api/review', 'GET /api/review']);
});

it('an empty queue is all clear', async () => {
  serve(() => [200, queue([])]);
  render(Review);
  expect(await screen.findByText('All clear')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Back to activity' })).toBeTruthy();
});

describe('the month’s AI allowance', () => {
  const nov1 = Date.UTC(2026, 10, 1) / 1000;
  const nov = new Date(nov1 * 1000).toLocaleDateString([], { day: 'numeric', month: 'long', timeZone: 'UTC' });
  const plan = (paused: boolean, upgrade: string | null): Limits => ({
    mailboxes: null,
    ai_emails: { used: 3000, limit: 3000, month: '2026-10', resets_at: nov1, counted: true, paused },
    upgrade_url: upgrade,
  });
  const daemon = (l: Limits) => serve((call) => (call === 'GET /api/limits' ? [200, l] : [200, queue([8])]));

  it.each([
    ['with an upgrade from the hosted service', '#/settings/plan', ['Add your own model key', 'upgrade']],
    ['with only the own key when it gives none', null, ['Add your own model key']],
  ])('used up, says why email waits here, %s', async (_name, upgrade, links) => {
    daemon(plan(true, upgrade));
    render(Review);
    const banner = await screen.findByRole('status');
    expect(banner.textContent).toContain(`This month's AI-sorted emails are used up, so until ${nov} the emails that need AI wait here for you.`);
    expect(within(banner).getAllByRole('link').map((a) => [a.textContent, a.getAttribute('href')])).toEqual(
      links.map((name) => [name, name === 'upgrade' ? upgrade : '#/settings']),
    );
    expect(await screen.findByRole('article')).toBeTruthy(); // the queue is shown below it
  });

  it('is not mentioned while there is some left', async () => {
    daemon(plan(false, '#/settings/plan'));
    render(Review);
    await screen.findByRole('article');
    expect(screen.queryByRole('status')).toBeNull();
  });
});

it('shows an email with its answers', async () => {
  serve(() => [200, queue([8])]);
  render(Review);
  const card = within(await screen.findByRole('article'));
  expect(card.getByText('Priya Nair')).toBeTruthy(); // the sender's name, when the email had one
  expect(card.getByText('Best guess: Recruiters · 0.58')).toBeTruthy();
  for (const name of ['Yes, Recruiters', 'Keep in Inbox', 'Apply']) expect(card.getByRole('button', { name })).toBeTruthy();
});

describe('Always do this', () => {
  const tooBroad = "Anyone can have an address at talentbridge.in, so a rule for the whole domain would catch mail from strangers. Make the rule for this sender's address instead.";
  /** The daemon with one email waiting; `broad` makes it refuse a rule for the whole domain. */
  const daemon = (broad = false) =>
    serve((call, body) =>
      call.startsWith('GET')
        ? [200, queue([8])]
        : broad && (body as { always_for?: string }).always_for === 'domain'
          ? [422, error('domain_too_broad', tooBroad, 'always_for')]
          : [200, { batch_id: 8, item: item({ id: 8 }) }],
    );
  const posts = (calls: { call: string; body?: unknown }[]) => calls.filter((c) => c.call.startsWith('POST')).map((c) => c.body);
  const tick = (card: ReturnType<typeof within>) => fireEvent.click(card.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }));

  it('is left out of the answer while the box is not ticked', async () => {
    const calls = daemon();
    render(Review);
    const card = within(await screen.findByRole('article'));
    expect(card.queryByRole('radio')).toBeNull();
    await fireEvent.click(card.getByRole('button', { name: 'Yes, Recruiters' }));
    await waitFor(() => expect(posts(calls)).toEqual([{ rule_id: 3 }]));
  });

  it.each([
    ['the whole domain by default', '', 'domain'],
    ['the address when chosen', 'Only priya@talentbridge.in', 'address'],
  ])('covers %s', async (_name, choose, want) => {
    const calls = daemon();
    render(Review);
    const card = within(await screen.findByRole('article'));
    await tick(card);
    expect((card.getByRole('radio', { name: 'Everyone at talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    if (choose) await fireEvent.click(card.getByRole('radio', { name: choose }));
    await fireEvent.click(card.getByRole('button', { name: 'Keep in Inbox' }));
    await waitFor(() => expect(posts(calls)).toEqual([{ rule_id: null, always_for: want }]));
  });

  it('a refused domain shows why, falls back to the address and waits for the button', async () => {
    const calls = daemon(true);
    render(Review);
    const card = within(await screen.findByRole('article'));
    await tick(card);
    await fireEvent.click(card.getByRole('button', { name: 'Yes, Recruiters' }));

    expect((await card.findByRole('alert')).textContent).toBe(tooBroad);
    expect((card.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    expect((card.getByRole('radio', { name: 'Only priya@talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    expect(posts(calls)).toEqual([{ rule_id: 3, always_for: 'domain' }]);

    await fireEvent.click(card.getByRole('button', { name: 'Yes, Recruiters' }));
    await waitFor(() => expect(posts(calls)[1]).toEqual({ rule_id: 3, always_for: 'address' }));
  });
});
