import * as activityApi from '../api/activity';
import { subscribe } from '../api/events';
import { flash } from './toast.svelte';

type Item = activityApi.ActivityItem;

/** How the row reads in the feed: sorted by a rule, sent to Trash, left alone, or waiting for the user. */
export type Kind = 'ok' | 'trash' | 'none' | 'review';

/** Empty string means "all". */
export interface Filter {
  rule: string;
  account: string;
  kind: Kind | '';
}

export const activity = $state({
  list: [] as Item[],
  /** Cursor of the next page; null at the end of the feed. */
  next: null as string | null,
  /** False until the first page has arrived, so "no mail yet" is never shown for "not loaded". */
  loaded: false,
  error: '',
  filter: { rule: '', account: '', kind: '' } as Filter,
  detail: null as activityApi.MessageDetail | null,
  detailError: '',
  /** Null until the tiles have loaded. */
  stats: null as activityApi.StatsSummary | null,
  statsError: '',
});

const message = (e: unknown) => (e instanceof Error ? e.message : String(e));

/** A failed action says why in the toast; the API's message is safe to show. */
export const failed = (e: unknown) => flash(message(e));

// The Outcome filter in the contract's parameters. "Sorted" is every email a rule was
// applied to, Trash included: the feed has no parameter for "acted on, but not trashed".
const outcomes: Record<Kind, activityApi.ActivityQuery> = {
  ok: { status: 'acted' },
  trash: { action: 'trash' },
  none: { stage: 'none' },
  review: { status: 'review' },
};

function params(cursor?: string | null): activityApi.ActivityQuery {
  const f = activity.filter;
  return { account: Number(f.account) || undefined, rule: Number(f.rule) || undefined, ...(f.kind && outcomes[f.kind]), cursor: cursor ?? undefined };
}

let asked = 0;

/** The first page for the filters in force; a slower earlier answer never replaces a later one. */
export async function load() {
  const mine = ++asked;
  try {
    const page = await activityApi.list(params());
    if (mine !== asked) return;
    activity.list = page.items;
    activity.next = page.next_cursor;
    activity.loaded = true;
    activity.error = '';
  } catch (e) {
    if (mine === asked) activity.error = message(e);
  }
}

export async function loadMore() {
  const mine = asked;
  try {
    const page = await activityApi.list(params(activity.next));
    if (mine !== asked) return;
    // A row that arrived live while this page was on its way is already in the list.
    activity.list.push(...page.items.filter((i) => !activity.list.some((r) => r.id === i.id)));
    activity.next = page.next_cursor;
  } catch (e) {
    failed(e);
  }
}

export async function loadStats() {
  try {
    activity.stats = await activityApi.summary();
    activity.statsError = '';
  } catch (e) {
    activity.statsError = message(e);
  }
}

let wanted = 0;

/** Fetch the decision trace and preview for one message; a slower earlier request never replaces a later one. */
export async function open(id: number) {
  wanted = id;
  try {
    const m = await activityApi.get(id);
    if (wanted !== id) return;
    activity.detail = m;
    activity.detailError = '';
  } catch (e) {
    if (wanted === id) activity.detailError = message(e);
  }
}

// ponytail: with a filter on, a live row is updated in place but never added, because the
// daemon does the filtering and the browser does not repeat its rules. Changing the filter
// refetches. If live rows under a filter matter, have the stream take the same parameters.
/** A new or re-decided row: replace it in place, or put it on top. */
export function upsert(row: Item) {
  const i = activity.list.findIndex((r) => r.id === row.id);
  const f = activity.filter;
  if (i >= 0) activity.list[i] = row;
  else if (!f.rule && !f.account && !f.kind) activity.list.unshift(row);
  if (activity.detail?.id === row.id) open(row.id);
}

/** The actions of the latest decision or correction. Earlier ones were undone by it or, in dry-run, never happened. */
function current(r: Item) {
  const last = r.actions.at(-1);
  return r.actions.filter((a) => a.batch_id === last?.batch_id && a.decision_id === last?.decision_id);
}

const inEffect = (r: Item) => r.actions.filter((a) => a.status === 'done' && a.kind !== 'review');

/** Everything the latest decision did has been undone. */
export const undone = (r: Item) => current(r).length > 0 && current(r).every((a) => a.status === 'undone');

/** The rule the email is filed under: the user's correction when there is one, else the decision. */
export const ruleName = (r: Item) => (r.state === 'review' ? 'Needs review' : (r.correction ?? r.decision)?.rule_name || 'No rule');

export const kind = (r: Item): Kind =>
  r.state === 'review'
    ? 'review'
    : current(r).some((a) => a.kind === 'trash' || a.kind === 'junk')
      ? 'trash'
      : ruleName(r) === 'No rule'
        ? 'none'
        : 'ok';

const words: Record<activityApi.MessageAction['kind'], string> = {
  move: 'moved',
  archive: 'archived',
  trash: 'moved to Trash',
  junk: 'moved to Junk',
  flag: 'flagged',
  unflag: 'unflagged',
  read: 'read',
  unread: 'unread',
  keep: 'kept in Inbox',
  review: 'in Inbox',
};

/** What happened to the email, in the feed's words: "Moved to Food · read". */
export function outcome(r: Item) {
  const acts = current(r);
  if (!acts.length) return r.state === 'skipped' ? 'Kept in Inbox' : 'In Inbox';
  if (undone(r)) return 'Undone · back in Inbox';
  const stuck = acts.find((a) => a.status === 'failed');
  if (stuck) return 'Failed: ' + stuck.error;
  const text = acts
    .filter((a) => a.status !== 'undone')
    .map((a) => (a.kind === 'move' ? 'moved to ' + a.folder : words[a.kind]))
    .join(' · ');
  return text[0].toUpperCase() + text.slice(1) + (acts.some((a) => a.status === 'dry_run') ? ' (dry run)' : '');
}

export const canUndo = (r: Item) => r.undoable;

function applyAction(a: activityApi.MessageAction) {
  const r = activity.list.find((x) => x.id === a.message_id);
  if (!r) return;
  const i = r.actions.findIndex((x) => x.id === a.id);
  if (i >= 0) r.actions[i] = a;
  r.undoable = inEffect(r).length > 0;
}

export async function undo(row: Item) {
  try {
    // The contract undoes one action at a time and a row can hold several (moved, then marked read): newest first.
    for (const a of inEffect(row).reverse()) applyAction(await activityApi.undo(a.id));
    flash('Undone. The email is back where it was');
  } catch (e) {
    failed(e);
  }
}

export async function undoLastHour() {
  try {
    const r = await activityApi.undoSince(Math.floor(Date.now() / 1000) - 3600);
    await load();
    const n = r.undone;
    flash(
      (n ? `Undid ${n} ${n === 1 ? 'action' : 'actions'}` : 'Nothing to undo') +
        ' from the last hour' +
        (r.failed ? `; ${r.failed} could not be undone` : ''),
    );
  } catch (e) {
    failed(e);
  }
}

/** ruleId null means keep in Inbox. */
export async function correct(id: number, ruleId: number | null, always: boolean) {
  try {
    const { item } = await activityApi.correct(id, { rule_id: ruleId, always_for_sender: always });
    upsert(item);
    flash(always ? 'Fixed, and saved as a sender rule for ' + item.from : 'Fixed. MailRules will use this as an example');
  } catch (e) {
    failed(e);
  }
}

subscribe('message.processed', upsert);
// Mail that goes to Needs review is announced by this event only.
subscribe('message.review', upsert);
subscribe('action.undone', applyAction);
