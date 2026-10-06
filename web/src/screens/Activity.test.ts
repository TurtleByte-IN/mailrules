import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { detail, error, inReview, item, serve, stats } from '../lib/state/activity.fixtures';
import { activity } from '../lib/state/activity.svelte';
import { review } from '../lib/state/review.svelte';
import Activity from './Activity.svelte';

// The state lives in module scope and a mounted screen cannot be given fresh modules, so each test starts it over.
beforeEach(() => {
  Object.assign(activity, { list: [], next: null, loaded: false, error: '', filter: { rule: '', account: '', outcome: '' }, detail: null, detailError: '', stats: null, statsError: '' });
  Object.assign(review, { list: [], next: null, total: 0, loaded: false, error: '' });
  go('#/activity');
});
afterEach(() => vi.unstubAllGlobals());

/** Stand on a URL the way the router sees it. */
function go(hash: string) {
  location.hash = hash;
  window.dispatchEvent(new Event('hashchange'));
}

const tooBroad = "Anyone can have an address at talentbridge.in, so a rule for the whole domain would catch mail from strangers. Make the rule for this sender's address instead.";
const down: [number, unknown] = [500, error('internal', 'Something went wrong.')];
const withCandidates = detail();
withCandidates.trace[0].candidates = [
  { rule_id: 3, rule_name: 'Recruiters', probability: 0.62 },
  { rule_id: null, rule_name: '', probability: 0.3 },
];

/** The daemon with a feed, three emails in review and today's numbers; `fail` names the route that is down. */
const daemon = (rows = [item()], fail = '') =>
  serve((call, body) =>
    fail && call.includes(fail)
      ? down
      : call.includes('/stats/summary')
        ? [200, stats]
        : call.includes('/review')
          ? [200, { items: [inReview(9)], next_cursor: null, total: 3 }]
          : call.includes('/messages/')
            ? call.startsWith('POST')
              ? fail === 'too broad' && (body as { always_for?: string }).always_for === 'domain'
                ? [422, error('domain_too_broad', tooBroad, 'always_for')]
                : [200, { batch_id: 8, item: item() }]
              : [200, { message: withCandidates }]
            : [200, { items: rows, next_cursor: null }],
  );

it.each([
  ['feed', '/activity', 'Activity feed'],
  ['tiles', '/stats/summary', ''],
])('a failed %s load is an alert, and Retry asks again', async (_name, route, inside) => {
  const calls = daemon([], route);
  render(Activity);
  const alert = await screen.findByRole('alert');
  expect(alert.textContent).toContain('Something went wrong.');
  // A feed that failed to load is not an empty feed.
  if (inside) {
    expect(screen.getByRole('region', { name: inside }).contains(alert)).toBe(true);
    expect(screen.queryByText('Nothing sorted yet. New mail shows up here as it arrives.')).toBeNull();
  }

  const asked = () => calls.filter((c) => c.call.includes(route)).length;
  expect(asked()).toBe(1);
  await fireEvent.click(within(alert).getByRole('button', { name: 'Retry' }));
  expect(asked()).toBe(2);
});

it('an empty feed says nothing is sorted yet', async () => {
  daemon([]);
  render(Activity);
  expect(await screen.findByText('Nothing sorted yet. New mail shows up here as it arrives.')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('shows the rows, the tiles and what the model gave each rule', async () => {
  daemon([item(), item({ id: 2, from: 'orders@swiggy.in', from_name: '', subject: 'Your order is on the way' })]);
  render(Activity);
  const feed = within(screen.getByRole('region', { name: 'Activity feed' }));
  expect(await feed.findByText('orders@swiggy.in')).toBeTruthy();
  expect(feed.getByText('Priya Nair')).toBeTruthy(); // the sender's name, when the email had one
  expect(feed.getAllByRole('button', { name: 'Undo' })).toHaveLength(2);

  const tiles = screen.getByRole('region', { name: 'Today at a glance' });
  await within(tiles).findByText('Sorted today');
  expect([...tiles.children].map((t) => t.textContent?.replace(/\s+/g, ' ').trim())).toEqual([
    'Sorted today 31 across 0 mailboxes',
    'Needs review 3 Still in your inbox · Review now',
    'Decided without a model 78% sender rules and conditions',
    'Model cost today $0.01 jev 31 calls',
  ]);

  const why = within(await screen.findByRole('complementary', { name: 'Decision details' }));
  expect(why.getByText('Recruiters 0.62')).toBeTruthy();
  expect(why.getByText('No rule 0.30')).toBeTruthy();
});

const feedCalls = (calls: { call: string }[]) => calls.map((c) => c.call).filter((c) => c.includes('/api/activity'));

it('the Outcome filter offers the four groups of the Overview and sends the one chosen', async () => {
  const calls = daemon();
  render(Activity);
  const select = screen.getByLabelText('Outcome') as HTMLSelectElement;
  expect([...select.options].map((o) => o.text)).toEqual(['All outcomes', 'Sorted', 'Left in Inbox', 'Needs review', 'Trashed']);

  await fireEvent.change(select, { target: { value: 'inbox' } });
  expect(feedCalls(calls)).toEqual(['GET /api/activity', 'GET /api/activity?outcome=inbox']);
});

it.each(['sorted', 'trashed', 'inbox', 'review'])('#/activity?outcome=%s opens the feed with that filter chosen and applied', async (outcome) => {
  go('#/activity?outcome=' + outcome);
  const calls = daemon();
  render(Activity);
  expect((screen.getByLabelText('Outcome') as HTMLSelectElement).value).toBe(outcome);
  expect(feedCalls(calls)).toEqual(['GET /api/activity?outcome=' + outcome]);
});

it('an unknown outcome in the URL is ignored', async () => {
  go('#/activity?outcome=spam');
  const calls = daemon();
  render(Activity);
  expect((screen.getByLabelText('Outcome') as HTMLSelectElement).value).toBe('');
  expect(feedCalls(calls)).toEqual(['GET /api/activity']);
});

it('a row the user had the last word on says whether it was a correction or a review answer', async () => {
  const word = (kind: 'correction' | 'review') => ({ kind, rule_id: 3, rule_name: 'Recruiters', created_at: 2000 });
  daemon([item({ correction: word('correction') }), item({ id: 2, correction: word('review') })]);
  render(Activity);
  const rows = await within(screen.getByRole('region', { name: 'Activity feed' })).findAllByRole('listitem');
  expect(within(rows[0]).getByText('you · corrected')).toBeTruthy();
  expect(within(rows[1]).getByText('you · reviewed')).toBeTruthy();
});

describe('Always do this', () => {
  const posts = (calls: { call: string; body?: unknown }[]) => calls.filter((c) => c.call.startsWith('POST')).map((c) => c.body);
  const panel = async () => within(await screen.findByRole('complementary', { name: 'Decision details' }));

  it('is left out of the fix while the box is not ticked', async () => {
    const calls = daemon();
    render(Activity);
    const why = await panel();
    expect(why.queryByRole('radio')).toBeNull();
    await fireEvent.click(why.getByRole('button', { name: 'Fix it' }));
    await waitFor(() => expect(posts(calls)).toEqual([{ rule_id: null }]));
  });

  it('covers the whole domain unless the address is chosen', async () => {
    const calls = daemon();
    render(Activity);
    const why = await panel();
    await fireEvent.click(why.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }));
    expect((why.getByRole('radio', { name: 'Everyone at talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    await fireEvent.click(why.getByRole('button', { name: 'Fix it' }));
    await waitFor(() => expect(posts(calls)).toEqual([{ rule_id: null, always_for: 'domain' }]));

    // A fix that went through unticks the box.
    await waitFor(() => expect(why.queryByRole('radio')).toBeNull());
    await fireEvent.click(why.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }));
    await fireEvent.click(why.getByRole('radio', { name: 'Only priya@talentbridge.in' }));
    await fireEvent.click(why.getByRole('button', { name: 'Fix it' }));
    await waitFor(() => expect(posts(calls)[1]).toEqual({ rule_id: null, always_for: 'address' }));
  });

  it('a refused domain shows why, falls back to the address and waits for the button', async () => {
    const calls = daemon([item()], 'too broad');
    render(Activity);
    const why = await panel();
    await fireEvent.click(why.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }));
    await fireEvent.click(why.getByRole('button', { name: 'Fix it' }));

    expect((await why.findByRole('alert')).textContent).toBe(tooBroad);
    expect((why.getByRole('checkbox', { name: 'Always do this for talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    expect((why.getByRole('radio', { name: 'Only priya@talentbridge.in' }) as HTMLInputElement).checked).toBe(true);
    expect(posts(calls)).toEqual([{ rule_id: null, always_for: 'domain' }]);

    await fireEvent.click(why.getByRole('button', { name: 'Fix it' }));
    await waitFor(() => expect(posts(calls)[1]).toEqual({ rule_id: null, always_for: 'address' }));
    await waitFor(() => expect(why.queryByRole('alert')).toBeNull());
  });
});
