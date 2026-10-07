import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { ActivityItem } from '../api/activity';
import { action, decision, detail, error, item, inReview, serve, stats } from './activity.fixtures';
import type { Filter } from './activity.svelte';

// Fresh modules per test: the state lives in module scope.
let s: typeof import('./activity.svelte');
let events: typeof import('../api/events');
let toast: { text: string };

beforeEach(async () => {
  vi.resetModules();
  s = await import('./activity.svelte');
  events = await import('../api/events');
  ({ toast } = await import('./toast.svelte'));
});
afterEach(() => vi.unstubAllGlobals());

const page = (items: ActivityItem[], next_cursor: string | null = null) => ({ items, next_cursor });
const ids = () => s.activity.list.map((r) => r.id);

describe('feed', () => {
  it('loads a page, then the next one from its cursor, skipping rows it already has', async () => {
    const calls = serve((call) => [200, call.includes('cursor=7') ? page([item({ id: 7 }), item({ id: 6 })]) : page([item({ id: 9 }), item({ id: 8 })], '7')]);
    await s.load();
    expect(s.activity).toMatchObject({ loaded: true, next: '7', error: '' });

    events.dispatch('message.processed', item({ id: 7 }));
    await s.loadMore();
    expect(ids()).toEqual([7, 9, 8, 6]);
    expect(s.activity.next).toBeNull();
    expect(calls.map((c) => c.call)).toEqual(['GET /api/activity', 'GET /api/activity?cursor=7']);
  });

  it.each<[string, Partial<Filter>, string]>([
    ['nothing', {}, ''],
    ['a rule', { rule: '3' }, '?rule=3'],
    ['a mailbox', { account: '2' }, '?account=2'],
    ['sorted', { outcome: 'sorted' }, '?outcome=sorted'],
    ['trashed', { outcome: 'trashed' }, '?outcome=trashed'],
    ['left in Inbox', { outcome: 'inbox' }, '?outcome=inbox'],
    ['needs review', { outcome: 'review' }, '?outcome=review'],
    ['a rule, a mailbox and an outcome', { rule: '3', account: '2', outcome: 'trashed' }, '?account=2&rule=3&outcome=trashed'],
  ])('sends a filter by %s to the daemon', async (_name, f, want) => {
    const calls = serve(() => [200, page([], '5')]);
    Object.assign(s.activity.filter, f);
    await s.load();
    await s.loadMore();
    expect(calls.map((c) => c.call)).toEqual(['GET /api/activity' + want, `GET /api/activity${want ? want + '&' : '?'}cursor=5`]);
  });

  it('a failed load is an error, not an empty feed, and a retry clears it', async () => {
    serve(() => [500, error('internal', 'Something went wrong.')]);
    await s.load();
    expect(s.activity).toMatchObject({ loaded: false, error: 'Something went wrong.', list: [] });

    serve(() => [200, page([])]);
    await s.load();
    expect(s.activity).toMatchObject({ loaded: true, error: '', list: [] });
  });

  it('a slower earlier answer never replaces a later one', async () => {
    let first!: (r: Response) => void;
    vi.stubGlobal('fetch', vi.fn()
      .mockImplementationOnce(() => new Promise<Response>((r) => (first = r)))
      .mockImplementationOnce(async () => new Response(JSON.stringify(page([item({ id: 2 })])))));
    const slow = s.load();
    await s.load();
    first(new Response(JSON.stringify(page([item({ id: 1 })]))));
    await slow;
    expect(ids()).toEqual([2]);
  });
});

describe('decision panel', () => {
  it('loads the message with its trace', async () => {
    const calls = serve(() => [200, { message: detail() }]);
    await s.open(1);
    expect(calls).toEqual([{ call: 'GET /api/messages/1' }]);
    expect(s.activity.detail?.trace.map((t) => t.kind)).toEqual(['decider', 'action']);
  });

  it('a failed load is an error', async () => {
    serve(() => [404, error('not_found', 'No such message.')]);
    await s.open(1);
    expect(s.activity).toMatchObject({ detail: null, detailError: 'No such message.' });
  });
});

describe('tiles', () => {
  it.each([
    ['a failure is an error', 500, error('internal', 'Something went wrong.'), null, 'Something went wrong.'],
    ['a summary', 200, stats, stats, ''],
  ])('%s', async (_name, status, body, want, said) => {
    const calls = serve(() => [status, body]);
    await s.loadStats();
    expect(calls).toEqual([{ call: 'GET /api/stats/summary?range=day' }]);
    expect(s.activity.stats).toEqual(want);
    expect(s.activity.statsError).toBe(said);
  });

  it('follow the feed once they are showing, and not before', async () => {
    const calls = serve(() => [200, stats]);
    events.dispatch('message.processed', item({ id: 7 }));
    expect(calls).toEqual([]);

    await s.loadStats();
    events.dispatch('message.processed', item({ id: 8 }));
    events.dispatch('action.undone', action({ message_id: 8, status: 'undone', undone_at: 2000 }));
    expect(calls.filter((c) => c.call.includes('/stats/summary'))).toHaveLength(3);
  });
});

describe('undo', () => {
  const two = () => item({ actions: [action({ id: 21 }), action({ id: 22, kind: 'read', folder: '', to_folder: '' })] });

  const after = (statuses: string[]) => {
    const row = two();
    row.actions.forEach((a, i) => Object.assign(a, { status: statuses[i] }));
    return { ...row, undoable: statuses.includes('done') };
  };

  it('undoes the whole email in one call and shows the row the daemon answers with', async () => {
    const calls = serve((call) => (call.startsWith('GET') ? [200, page([two()])] : [200, { batch_id: 9, item: after(['undone', 'undone']), undone: 2, failed: 0 }]));
    await s.load();
    await s.undo(s.activity.list[0]);
    expect(calls.slice(1)).toEqual([{ call: 'POST /api/messages/' + two().id + '/undo' }]);
    expect(s.canUndo(s.activity.list[0])).toBe(false);
    expect(s.activity.list[0].actions.map((a) => a.status)).toEqual(['undone', 'undone']);
    expect(toast.text).toBe('Undone. The email is back where it was');
  });

  it('says so when only part of it could be undone, and the row stays undoable', async () => {
    serve((call) => (call.startsWith('GET') ? [200, page([two()])] : [200, { batch_id: 9, item: after(['done', 'undone']), undone: 1, failed: 1 }]));
    await s.load();
    await s.undo(s.activity.list[0]);
    expect(s.canUndo(s.activity.list[0])).toBe(true);
    expect(toast.text).toBe('Undid 1 of 2 actions; the rest could not be undone');
  });

  it('shows why when the daemon refuses, and leaves the row as it was', async () => {
    serve((call) => (call.startsWith('GET') ? [200, page([two()])] : [409, error('message_gone', 'This email is no longer in the mailbox.')]));
    await s.load();
    await s.undo(s.activity.list[0]);
    expect(toast.text).toBe('This email is no longer in the mailbox.');
    expect(s.canUndo(s.activity.list[0])).toBe(true);
  });

  it('says so when it was done too long ago (409 too_old), and leaves the row as it was', async () => {
    const said = 'This was done more than 30 days ago, so it can no longer be undone.';
    serve((call) => (call.startsWith('GET') ? [200, page([two()])] : [409, error('too_old', said)]));
    await s.load();
    await s.undo(s.activity.list[0]);
    expect(toast.text).toBe(said);
    expect(s.activity.list).toEqual([two()]);
  });
});

describe('undoLastHour', () => {
  it.each([
    [0, 0, 'Nothing to undo from the last hour'],
    [1, 0, 'Undid 1 action from the last hour'],
    [6, 2, 'Undid 6 actions from the last hour; 2 could not be undone'],
  ])('%i undone, %i failed', async (undone, failed, said) => {
    vi.spyOn(Date, 'now').mockReturnValue(10_000_000);
    const batch = { id: 9, kind: 'undo', status: 'done', total: null, done: undone, created_at: 10_000, actions: { done: 0, dry_run: 0, failed: 0, undone: 0 } };
    const calls = serve((call) => (call.startsWith('POST') ? [200, { batch, undone, failed }] : [200, page([item({ undoable: false })])]));
    await s.undoLastHour();
    expect(calls).toEqual([{ call: 'POST /api/actions/undo?since=6400' }, { call: 'GET /api/activity' }]);
    expect(ids()).toEqual([1]);
    expect(toast.text).toBe(said);
    vi.restoreAllMocks();
  });

  it('shows why when it fails', async () => {
    serve(() => [400, error('invalid_input', 'since must be a positive integer.')]);
    await s.undoLastHour();
    expect(toast.text).toBe('since must be a positive integer.');
  });
});

describe('correct', () => {
  const fixed = item({ correction: { kind: 'correction', rule_id: 4, rule_name: 'Scams', created_at: 2000 }, actions: [action({ status: 'undone' }), action({ id: 30, decision_id: null, batch_id: 8, kind: 'trash', folder: '' })] });

  it.each([
    { to: 4, always: undefined, body: { rule_id: 4 }, said: 'Fixed. MailRules will use this as an example' },
    { to: null, always: undefined, body: { rule_id: null }, said: 'Fixed. MailRules will use this as an example' },
    { to: 4, always: 'domain' as const, body: { rule_id: 4, always_for: 'domain' }, said: 'Fixed, and saved as a sender rule for talentbridge.in' },
    { to: 4, always: 'address' as const, body: { rule_id: 4, always_for: 'address' }, said: 'Fixed, and saved as a sender rule for priya@talentbridge.in' },
  ])('to $to, always $always', async (c) => {
    const calls = serve((call) => (call.startsWith('GET') ? [200, page([item()])] : [200, { batch_id: 8, item: fixed }]));
    await s.load();
    await s.correct(1, c.to, c.always);
    expect(calls[1]).toEqual({ call: 'POST /api/messages/1/correct', body: c.body });
    expect(s.activity.list).toHaveLength(1);
    expect(s.ruleName(s.activity.list[0])).toBe('Scams');
    expect(toast.text).toBe(c.said);
  });

  it('refetches the open decision trace', async () => {
    const calls = serve((call) => (call.includes('/correct') ? [200, { batch_id: 8, item: fixed }] : [200, { message: detail() }]));
    await s.open(1);
    await s.correct(1, 4);
    expect(calls.map((c) => c.call)).toEqual(['GET /api/messages/1', 'POST /api/messages/1/correct', 'GET /api/messages/1']);
  });

  it('shows why when the daemon refuses', async () => {
    serve(() => [409, error('account_offline', 'This mailbox is not connected.')]);
    expect(await s.correct(1, 4)).toBe('');
    expect(toast.text).toBe('This mailbox is not connected.');
  });

  it('says so when the account has no folder for the rule (422), and leaves the row as it was', async () => {
    const said = 'This mail account has no Trash folder, so that action cannot be carried out. Choose a rule that moves the mail to a named folder instead.';
    serve((call) => (call.startsWith('GET') ? [200, page([item()])] : [422, error('no_special_folder', said)]));
    await s.load();
    await s.correct(1, 4);
    expect(toast.text).toBe(said);
    expect(s.activity.list).toEqual([item()]);
  });

  it('hands back the refusal of a whole free-mail domain (422) for the form, without a toast', async () => {
    const said = "Anyone can have an address at gmail.com, so a rule for the whole domain would catch mail from strangers. Make the rule for this sender's address instead.";
    serve((call) => (call.startsWith('GET') ? [200, page([item()])] : [422, error('domain_too_broad', said, 'always_for')]));
    await s.load();
    expect(await s.correct(1, 4, 'domain')).toBe(said);
    expect(toast.text).toBe('');
    expect(s.activity.list).toEqual([item()]);
  });
});

describe('live events', () => {
  beforeEach(async () => {
    serve(() => [200, page([item({ id: 3 }), item({ id: 2 }), item({ id: 1 })])]);
    await s.load();
  });

  it('message.processed puts a new row on top and replaces a known one in place', () => {
    events.dispatch('message.processed', item({ id: 4 }));
    events.dispatch('message.processed', item({ id: 2, subject: 'Changed' }));
    expect(ids()).toEqual([4, 3, 2, 1]);
    expect(s.activity.list[2].subject).toBe('Changed');
  });

  it('message.review puts the waiting email in the feed', () => {
    events.dispatch('message.review', inReview(4));
    expect(ids()).toEqual([4, 3, 2, 1]);
    expect(s.kind(s.activity.list[0])).toBe('review');
  });

  it('with a filter on, a live row is updated in place but not added', () => {
    s.activity.filter.outcome = 'trashed';
    events.dispatch('message.processed', item({ id: 4 }));
    events.dispatch('message.processed', item({ id: 2, subject: 'Changed' }));
    expect(ids()).toEqual([3, 2, 1]);
    expect(s.activity.list[1].subject).toBe('Changed');
  });

  it('action.undone marks the action on the row that owns it', () => {
    events.dispatch('action.undone', action({ message_id: 2, status: 'undone', undone_at: 2000 }));
    expect(s.activity.list.map((r) => s.canUndo(r))).toEqual([true, false, true]);
    expect(s.outcome(s.activity.list[1])).toBe('Undone · back in Inbox');
  });
});

describe('how a row reads', () => {
  it.each<[string, Partial<ActivityItem>, string, string]>([
    ['moved and marked read', {}, 'Recruiters', 'ok'],
    ['trashed', { decision: decision({ rule_name: 'Scams' }), actions: [action({ kind: 'trash', folder: '' })] }, 'Scams', 'trash'],
    ['no rule', { state: 'skipped', decision: decision({ stage: 'none', rule_id: null, rule_name: '' }), actions: [], undoable: false }, 'No rule', 'none'],
    ['a deleted rule', { decision: decision({ rule_id: null, rule_name: 'Old rule' }) }, 'Old rule', 'ok'],
    ['waiting for review', inReview(1), 'Needs review', 'review'],
    [
      'corrected to keep in Inbox: the correction overrules the decision and its actions',
      { correction: { kind: 'correction', rule_id: null, rule_name: '', created_at: 2000 }, actions: [action({ status: 'undone' }), action({ id: 30, decision_id: null, batch_id: 8, kind: 'keep', folder: '', to_folder: 'INBOX' })] },
      'No rule', 'none',
    ],
    ['not decided yet', { state: 'new', decision: null, actions: [], undoable: false }, 'No rule', 'none'],
  ])('%s', (_name, over, rule, kind) => {
    const r = item(over);
    expect([s.ruleName(r), s.kind(r)]).toEqual([rule, kind]);
  });

  it('the outcome is the sentence the daemon sent', () => {
    expect(s.outcome(item({ outcome: 'Would move to Jobs · mark read' }))).toBe('Would move to Jobs · mark read');
    expect(s.outcome(item({ outcome: 'Moved to Jobs', actions: [action({ status: 'undone' })] }))).toBe('Undone · back in Inbox');
  });
});
