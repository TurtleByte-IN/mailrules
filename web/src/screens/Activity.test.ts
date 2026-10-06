import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { detail, error, inReview, item, serve, stats } from '../lib/state/activity.fixtures';
import { activity } from '../lib/state/activity.svelte';
import { review } from '../lib/state/review.svelte';
import Activity from './Activity.svelte';

// The state lives in module scope and a mounted screen cannot be given fresh modules, so each test starts it over.
beforeEach(() => {
  Object.assign(activity, { list: [], next: null, loaded: false, error: '', detail: null, detailError: '', stats: null, statsError: '' });
  Object.assign(review, { list: [], next: null, total: 0, loaded: false, error: '' });
});
afterEach(() => vi.unstubAllGlobals());

const down: [number, unknown] = [500, error('internal', 'Something went wrong.')];
const withCandidates = detail();
withCandidates.trace[0].candidates = [
  { rule_id: 3, rule_name: 'Recruiters', probability: 0.62 },
  { rule_id: null, rule_name: '', probability: 0.3 },
];

/** The daemon with a feed, three emails in review and today's numbers; `fail` names the route that is down. */
const daemon = (rows = [item()], fail = '') =>
  serve((call) =>
    fail && call.includes(fail)
      ? down
      : call.includes('/stats/summary')
        ? [200, stats]
        : call.includes('/review')
          ? [200, { items: [inReview(9)], next_cursor: null, total: 3 }]
          : call.includes('/messages/')
            ? [200, { message: withCandidates }]
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
  daemon([item(), item({ id: 2, from: 'orders@swiggy.in', subject: 'Your order is on the way' })]);
  render(Activity);
  const feed = within(screen.getByRole('region', { name: 'Activity feed' }));
  expect(await feed.findByText('orders@swiggy.in')).toBeTruthy();
  expect(feed.getByText('priya@talentbridge.in')).toBeTruthy();
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
