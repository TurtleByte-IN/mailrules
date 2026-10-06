import * as activityApi from '../api/activity';
import { subscribe } from '../api/events';
import { flash } from './toast.svelte';

/** Empty string means "all". */
export interface Filter {
  rule: string;
  account: string;
  kind: activityApi.Kind | '';
}

export const activity = $state<{ list: activityApi.ActivityRow[]; filter: Filter; detail: activityApi.Message | null }>({
  list: [],
  filter: { rule: '', account: '', kind: '' },
  detail: null,
});

// ponytail: filters one loaded page in the browser. When the feed is wired, send them as
// GET /api/activity?account=&rule=… with the cursor, and keep this for rows that arrive live.
export const matches = (r: activityApi.ActivityRow, f: Filter) =>
  (!f.rule || r.ruleId === f.rule) && (!f.account || r.accountId === f.account) && (!f.kind || r.kind === f.kind);

export async function load() {
  activity.list = await activityApi.list();
}

let wanted = '';

/** Fetch the decision trace and preview for one message; a slower earlier request never replaces a later one. */
export async function open(id: string) {
  wanted = id;
  const m = await activityApi.get(id);
  if (wanted === id) activity.detail = m;
}

/** A new or re-decided row: replace it in place, or put it on top. */
export function upsert(row: activityApi.ActivityRow) {
  const i = activity.list.findIndex((r) => r.id === row.id);
  if (i < 0) activity.list.unshift(row);
  else activity.list[i] = row;
  if (activity.detail?.id === row.id) open(row.id);
}

function markUndone(actionIds: string[]) {
  for (const r of activity.list) if (r.actionId && actionIds.includes(r.actionId)) r.undone = true;
}

export const canUndo = (r: activityApi.ActivityRow) => r.actionId !== null && !r.undone && r.kind !== 'review';

export async function undo(row: activityApi.ActivityRow) {
  if (!row.actionId) return;
  await activityApi.undo(row.actionId);
  markUndone([row.actionId]);
  flash('Undone. The email is back where it was');
}

export async function undoLastHour() {
  const ids = await activityApi.undoSince(Math.floor(Date.now() / 1000) - 3600);
  markUndone(ids);
  const n = ids.length;
  flash((n ? `Undid ${n} ${n === 1 ? 'action' : 'actions'}` : 'Nothing to undo') + ' from the last hour');
}

/** ruleId null means keep in Inbox. */
export async function correct(id: string, ruleId: string | null, always: boolean) {
  const row = await activityApi.correct(id, ruleId, always);
  upsert(row);
  flash(always ? 'Fixed, and saved as a sender rule for ' + row.domain : 'Fixed. MailRules will use this as an example');
}

subscribe('message.processed', upsert);
subscribe('action.undone', (e) => markUndone([e.actionId]));
