import { fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { error, inReview, serve } from '../lib/state/activity.fixtures';
import { review } from '../lib/state/review.svelte';
import Review from './Review.svelte';

// The state lives in module scope and a mounted screen cannot be given fresh modules, so each test starts it over.
beforeEach(() => {
  Object.assign(review, { list: [], next: null, total: 0, loaded: false, error: '' });
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
  expect(calls.map((c) => c.call)).toEqual(['GET /api/review', 'GET /api/review']);
});

it('an empty queue is all clear', async () => {
  serve(() => [200, queue([])]);
  render(Review);
  expect(await screen.findByText('All clear')).toBeTruthy();
  expect(screen.getByRole('link', { name: 'Back to activity' })).toBeTruthy();
});

it('shows an email with its answers', async () => {
  serve(() => [200, queue([8])]);
  render(Review);
  const card = within(await screen.findByRole('article'));
  expect(card.getByText('priya@talentbridge.in')).toBeTruthy();
  expect(card.getByText('Best guess: Recruiters · 0.58')).toBeTruthy();
  for (const name of ['Yes, Recruiters', 'Keep in Inbox', 'Apply']) expect(card.getByRole('button', { name })).toBeTruthy();
});
