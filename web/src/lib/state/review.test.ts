import { beforeEach, describe, expect, it, vi } from 'vitest';

// Fresh modules per test: the demo data lives in module scope and the actions change it.
let s: typeof import('./review.svelte');
let a: typeof import('./activity.svelte');
let events: typeof import('../api/events');
let badges: Record<string, number>;
let toast: { text: string };

beforeEach(async () => {
  vi.resetModules();
  s = await import('./review.svelte');
  a = await import('./activity.svelte');
  events = await import('../api/events');
  ({ badges } = await import('./badges.svelte'));
  ({ toast } = await import('./toast.svelte'));
  await Promise.all([s.load(), a.load()]);
});

describe('review queue', () => {
  it('writes the queue length to the nav badge', () => {
    expect(s.review.list).toHaveLength(4);
    expect(badges['/review']).toBe(4);
  });

  it.each([
    { name: 'approve the guess, row already in the feed', id: 'a8', to: 'r3', always: false, feed: 8, row: { rule: 'Recruiters', kind: 'ok', outcome: 'Moved to Jobs', reason: 'You chose Recruiters' }, undo: true, said: 'Done: Moved to Jobs' },
    { name: 'keep in Inbox, row not in the feed yet', id: 'v2', to: null, always: false, feed: 9, row: { rule: 'No rule', kind: 'none', outcome: 'Kept in Inbox', reason: 'You kept it in Inbox' }, undo: false, said: 'Kept in Inbox' },
    { name: 'another rule, always', id: 'v3', to: 'r4', always: true, feed: 9, row: { rule: 'Scams', kind: 'trash', outcome: 'Moved to Trash', reason: 'You chose Scams; saved as a sender rule' }, undo: true, said: 'Done: Moved to Trash' },
  ])('$name', async (c) => {
    await s.resolve(c.id, c.to, c.always);

    expect(s.review.list.map((x) => x.id)).not.toContain(c.id);
    expect(badges['/review']).toBe(3);
    expect(toast.text).toBe(c.said);

    const row = a.activity.list.find((r) => r.id === c.id)!;
    expect(a.activity.list).toHaveLength(c.feed);
    expect(row).toMatchObject({ ...c.row, ruleId: c.to, stage: 'reviewed', undone: false });
    expect(a.canUndo(row)).toBe(c.undo);
    if (c.feed === 9) expect(a.activity.list[0]).toBe(row);
  });

  it('counts down to an empty queue', async () => {
    for (const id of ['a8', 'v2', 'v3', 'v4']) await s.resolve(id, null, false);
    expect(s.review.list).toEqual([]);
    expect(badges['/review']).toBe(0);
  });

  it('message.review adds to the queue once and updates the badge', () => {
    const item = { ...s.review.list[0], id: 'new1' };
    events.dispatch('message.review', item);
    events.dispatch('message.review', item);
    expect(s.review.list.map((x) => x.id)).toEqual(['new1', 'a8', 'v2', 'v3', 'v4']);
    expect(badges['/review']).toBe(5);
  });
});
