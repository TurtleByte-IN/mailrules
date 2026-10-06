import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { ActivityRow } from '../api/activity';
import type { Filter } from './activity.svelte';

// Fresh modules per test: the demo data lives in module scope and the actions change it.
let s: typeof import('./activity.svelte');
let events: typeof import('../api/events');
let toast: { text: string };

beforeEach(async () => {
  vi.resetModules();
  s = await import('./activity.svelte');
  events = await import('../api/events');
  ({ toast } = await import('./toast.svelte'));
  await s.load();
});

const row = (id: string) => s.activity.list.find((r) => r.id === id)!;

describe('undo', () => {
  it('marks the row undone and says so', async () => {
    await s.undo(row('a2'));
    expect(row('a2').undone).toBe(true);
    expect(s.canUndo(row('a2'))).toBe(false);
    expect(toast.text).toBe('Undone. The email is back where it was');
  });

  it.each([
    ['a row a rule acted on', 'a1', true],
    ['mail no rule touched', 'a6', false],
    ['mail waiting for review', 'a8', false],
  ])('is offered for %s: %s → %s', (_name, id, want) => {
    expect(s.canUndo(row(id))).toBe(want);
  });
});

describe('undoLastHour', () => {
  it('undoes every action from the last hour, then has nothing left', async () => {
    await s.undoLastHour();
    expect(s.activity.list.filter((r) => r.undone).map((r) => r.id)).toEqual(['a1', 'a2', 'a3', 'a4', 'a5', 'a7']);
    expect(toast.text).toBe('Undid 6 actions from the last hour');

    await s.undoLastHour();
    expect(toast.text).toBe('Nothing to undo from the last hour');

    await s.correct('a2', 'r1', false);
    await s.undoLastHour();
    expect(toast.text).toBe('Undid 1 action from the last hour');
  });
});

describe('correct', () => {
  it.each([
    { to: 'r1', always: false, rule: 'Food orders', kind: 'ok', outcome: 'Moved to Food · read', undo: true, reason: 'Corrected by you', said: 'Fixed. MailRules will use this as an example' },
    { to: 'r4', always: false, rule: 'Scams', kind: 'trash', outcome: 'Moved to Trash', undo: true, reason: 'Corrected by you', said: 'Fixed. MailRules will use this as an example' },
    { to: null, always: false, rule: 'No rule', kind: 'none', outcome: 'Kept in Inbox', undo: false, reason: 'Corrected by you', said: 'Fixed. MailRules will use this as an example' },
    { to: 'r5', always: true, rule: 'Newsletters', kind: 'ok', outcome: 'Moved to Reading · read', undo: true, reason: 'Corrected by you; future mail from talentbridge.in follows this', said: 'Fixed, and saved as a sender rule for talentbridge.in' },
  ])('to $to, always $always', async (c) => {
    await s.undo(row('a2'));
    await s.correct('a2', c.to, c.always);
    expect(row('a2')).toMatchObject({ ruleId: c.to, rule: c.rule, kind: c.kind, outcome: c.outcome, stage: 'corrected', reason: c.reason, undone: false });
    expect(s.canUndo(row('a2'))).toBe(c.undo);
    expect(toast.text).toBe(c.said);
  });

  it('adds the correction to the open decision trace', async () => {
    await s.open('a2');
    await s.correct('a2', 'r1', true);
    await vi.waitFor(() => expect(s.activity.detail?.trace).toHaveLength(5));
    expect(s.activity.detail?.trace.at(-1)).toEqual({ label: 'Your correction', detail: 'Moved to Food orders', meta: 'Saved as a sender rule', active: true });
    expect(s.activity.detail?.trace.filter((t) => t.active)).toHaveLength(1);
  });
});

describe('filters', () => {
  const none: Filter = { rule: '', account: '', kind: '' };
  it.each<[string, Partial<Filter>, string[]]>([
    ['nothing', {}, ['a1', 'a2', 'a3', 'a4', 'a5', 'a6', 'a7', 'a8']],
    ['a rule', { rule: 'r3' }, ['a2']],
    ['a mailbox', { account: 'acc2' }, ['a4', 'a6']],
    ['an outcome', { kind: 'trash' }, ['a3']],
    ['needs review', { kind: 'review' }, ['a8']],
    ['a rule and a mailbox', { rule: 'r5', account: 'acc2' }, ['a4']],
    ['a pair nothing matches', { rule: 'r5', account: 'acc1' }, []],
  ])('by %s', (_name, f, want) => {
    expect(s.activity.list.filter((r) => s.matches(r, { ...none, ...f })).map((r) => r.id)).toEqual(want);
  });
});

describe('live events', () => {
  it('message.processed puts a new row on top and replaces a known one in place', () => {
    const fresh: ActivityRow = { ...row('a1'), id: 'new1', subject: 'Fresh' };
    events.dispatch('message.processed', fresh);
    expect(s.activity.list[0].id).toBe('new1');
    expect(s.activity.list).toHaveLength(9);

    events.dispatch('message.processed', { ...row('a3'), outcome: 'Changed' });
    expect(s.activity.list).toHaveLength(9);
    expect(s.activity.list[3]).toMatchObject({ id: 'a3', outcome: 'Changed' });
  });

  it('action.undone marks the row that owns the action', () => {
    events.dispatch('action.undone', { actionId: 'act3' });
    expect(s.activity.list.filter((r) => r.undone).map((r) => r.id)).toEqual(['a3']);
  });
});
