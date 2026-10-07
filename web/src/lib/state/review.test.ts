import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { action, error, inReview, item, serve } from './activity.fixtures';

// Fresh modules per test: the state lives in module scope.
let s: typeof import('./review.svelte');
let a: typeof import('./activity.svelte');
let events: typeof import('../api/events');
let badges: Record<string, number>;
let toast: { text: string };

const queue = (ids: number[], total: number, next_cursor: string | null = null) => ({ items: ids.map(inReview), next_cursor, total });
const ids = () => s.review.list.map((x) => x.id);

beforeEach(async () => {
  vi.resetModules();
  s = await import('./review.svelte');
  a = await import('./activity.svelte');
  events = await import('../api/events');
  ({ badges } = await import('./badges.svelte'));
  ({ toast } = await import('./toast.svelte'));
});
afterEach(() => vi.unstubAllGlobals());

describe('review queue', () => {
  it('loads a page, writes the size of the whole queue to the nav badge, then loads the next page', async () => {
    const calls = serve((call) => [200, call.includes('cursor') ? queue([7, 6], 4) : queue([9, 8], 4, '8')]);
    await s.load();
    expect(s.review).toMatchObject({ loaded: true, total: 4, next: '8', error: '' });
    expect(badges['/review']).toBe(4);

    await s.loadMore();
    expect(ids()).toEqual([9, 8, 7, 6]);
    expect(s.review.next).toBeNull();
    expect(calls.map((c) => c.call)).toEqual(['GET /api/review', 'GET /api/review?cursor=8']);
  });

  it('an empty queue is loaded and empty', async () => {
    serve(() => [200, queue([], 0)]);
    await s.load();
    expect(s.review).toMatchObject({ loaded: true, list: [], total: 0, error: '' });
    expect(badges['/review']).toBe(0);
  });

  it('a failed load is an error, not an empty queue', async () => {
    serve(() => [500, error('internal', 'Something went wrong.')]);
    await s.load();
    expect(s.review).toMatchObject({ loaded: false, error: 'Something went wrong.' });
  });

  it.each([
    { name: 'approve the guess', to: 3, always: undefined, body: { rule_id: 3 }, said: 'Done: Moved to Jobs' },
    { name: 'another rule, always for the domain', to: 4, always: 'domain' as const, body: { rule_id: 4, always_for: 'domain' }, said: 'Done: Moved to Jobs' },
    { name: 'another rule, always for the address', to: 4, always: 'address' as const, body: { rule_id: 4, always_for: 'address' }, said: 'Done: Moved to Jobs' },
    { name: 'keep in Inbox', to: null, always: undefined, body: { rule_id: null }, said: 'Kept in Inbox' },
  ])('$name', async (c) => {
    const settled = item({ id: 8, correction: { kind: 'review', rule_id: c.to, rule_name: 'Recruiters', created_at: 2000 }, actions: [action({ message_id: 8, decision_id: null })] });
    const calls = serve((call) => (call.startsWith('GET') ? [200, queue([9, 8], 2)] : [200, { batch_id: 8, item: settled }]));
    await s.load();
    await s.resolve(8, c.to, c.always);

    expect(calls[1]).toEqual({ call: 'POST /api/review/8/resolve', body: c.body });
    expect(ids()).toEqual([9]);
    expect(badges['/review']).toBe(1);
    expect(toast.text).toBe(c.said);
    expect(a.activity.list.map((r) => r.id)).toEqual([8]);
  });

  it('shows why when the daemon refuses, and keeps the email in the queue', async () => {
    serve((call) => (call.startsWith('GET') ? [200, queue([9, 8], 2)] : [409, error('not_in_review', 'This message is not waiting in Needs review.')]));
    await s.load();
    await s.resolve(8, 3);
    expect(toast.text).toBe('This message is not waiting in Needs review.');
    expect(ids()).toEqual([9, 8]);
    expect(badges['/review']).toBe(2);
  });

  it('takes off the queue an email moved or deleted outside MailRules (409 message_gone), and says so', async () => {
    const said = 'This email was moved or deleted outside MailRules, so it was taken off the list.';
    serve((call) => (call.startsWith('GET') ? [200, queue([9, 8], 2)] : [409, error('message_gone', said)]));
    await s.load();
    expect(await s.resolve(8, 3)).toBe('');
    expect(toast.text).toBe(said);
    expect(ids()).toEqual([9]);
    expect(badges['/review']).toBe(1);
  });

  it('says so when the account has no folder for the rule (422), and keeps the email in the queue', async () => {
    const said = 'This mail account has no Archive folder, so that action cannot be carried out. Choose a rule that moves the mail to a named folder instead.';
    serve((call) => (call.startsWith('GET') ? [200, queue([9, 8], 2)] : [422, error('no_special_folder', said)]));
    await s.load();
    await s.resolve(8, 3);
    expect(toast.text).toBe(said);
    expect(ids()).toEqual([9, 8]);
    expect(badges['/review']).toBe(2);
  });

  it('hands back the refusal of a whole free-mail domain (422) for the card, without a toast, and keeps the email in the queue', async () => {
    const said = "Anyone can have an address at gmail.com, so a rule for the whole domain would catch mail from strangers. Make the rule for this sender's address instead.";
    serve((call) => (call.startsWith('GET') ? [200, queue([9, 8], 2)] : [422, error('domain_too_broad', said, 'always_for')]));
    await s.load();
    expect(await s.resolve(8, 3, 'domain')).toBe(said);
    expect(toast.text).toBe('');
    expect(ids()).toEqual([9, 8]);
  });

  describe('live events', () => {
    beforeEach(async () => {
      serve(() => [200, queue([9, 8], 2)]);
      await s.load();
    });

    it('message.review adds to the queue once and updates the badge', () => {
      events.dispatch('message.review', inReview(10));
      events.dispatch('message.review', inReview(10));
      expect(ids()).toEqual([10, 9, 8]);
      expect(badges['/review']).toBe(3);
    });

    it('message.processed takes a settled email out of the queue once', () => {
      events.dispatch('message.processed', item({ id: 8 }));
      events.dispatch('message.processed', item({ id: 8 }));
      events.dispatch('message.processed', item({ id: 99 }));
      expect(ids()).toEqual([9]);
      expect(badges['/review']).toBe(1);
    });
  });
});
