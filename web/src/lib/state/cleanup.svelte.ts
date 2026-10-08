import { ApiError } from '../api/client';
import * as cleanupApi from '../api/cleanup';
import { subscribe } from '../api/events';
import { day } from '../format';
import { scopeProblem, startScope, toRequest, type Scope } from '../scope';
import { accounts } from './accounts.svelte';
import { rules } from './rules.svelte';
import { settings } from './settings.svelte';
import { flash } from './toast.svelte';

// idle → checking (real checks are running) → ready/stale (rows shown) or failed.
// From ready, Sort moves to sorting → done. Changing what is picked goes back to idle unless a check
// or a sort is running or shown, so what the user sorts always matches the checks they are looking at.
type Phase = 'idle' | 'checking' | 'ready' | 'stale' | 'failed' | 'sorting' | 'done';

/**
 * One mailbox of the run on show: its check (progress while running, rows when ready or stale), which of
 * its rows the user unticked, and its batch once the run is sorting. A run over one mailbox has one lane.
 */
export interface Lane {
  accountId: string;
  check: cleanupApi.CleanupCheck | null;
  /** Indices of selectable rows the user unticked; empty = every selectable row is sorted. */
  excluded: Set<number>;
  batch: cleanupApi.Batch | null;
}

export const cleanup = $state<{
  phase: Phase;
  /** The folder and range of the run; `accountId` is the mailbox on show: its chart and table. */
  scope: Scope;
  /** The mailboxes picked, in the order the mailbox list has them. */
  mailboxes: string[];
  /** The rules picked, checked in their usual order; null = every rule. Sender rules apply either way. */
  ruleIds: number[] | null;
  /** Folders of the mailbox on show. Empty when several mailboxes are picked: they share only their Inbox. */
  folders: cleanupApi.Folder[];
  /** The run on show: one lane per mailbox, empty while idle. */
  lanes: Lane[];
  /** Past runs, newest first. */
  batches: cleanupApi.Batch[];
  /** Cursor of the next page of past runs; null at the end. */
  next: string | null;
  status: 'loading' | 'ready' | 'error';
  error: string;
}>({
  phase: 'idle',
  scope: startScope(),
  mailboxes: [],
  ruleIds: null,
  folders: [],
  lanes: [],
  batches: [],
  next: null,
  status: 'loading',
  error: '',
});

const POLL_MS = 2000;

let batchTimer: ReturnType<typeof setInterval> | undefined;
let checkTimer: ReturnType<typeof setInterval> | undefined;

const fail = (e: unknown) => flash((e as Error).message);

/** The lane of a mailbox of the run on show. */
export const laneOf = (accountId: string | number) => cleanup.lanes.find((l) => l.accountId === String(accountId));
/** The mailbox on show: the lane whose chart and table the screen draws. */
export const focused = () => laneOf(cleanup.scope.accountId) ?? cleanup.lanes[0] ?? null;

const newLane = (accountId: string): Lane => ({ accountId, check: null, excluded: new Set(), batch: null });

/** The run is over: no lanes, nothing shown. */
function clearRun() {
  stopCheckPoll();
  dropSelectionSave();
  cleanup.lanes = [];
  cleanup.phase = 'idle';
}

export async function load() {
  try {
    const page = await cleanupApi.list();
    cleanup.batches = page.items;
    cleanup.next = page.next_cursor;
    cleanup.status = 'ready';
    // A sort that was going before the page loaded is followed like one started here.
    const going = page.items.filter((b) => b.status === 'running' && b.account_id !== null);
    if (going.length && cleanup.phase !== 'sorting') {
      cleanup.mailboxes = going.map((b) => String(b.account_id));
      cleanup.scope.accountId = cleanup.mailboxes[0];
      cleanup.lanes = going.map((b) => ({ ...newLane(String(b.account_id)), batch: b }));
      follow();
    }
  } catch (e) {
    cleanup.status = 'error';
    cleanup.error = (e as Error).message;
  }
  // Restore the checks when returning to the page.
  await loadChecks();
}

export async function more() {
  try {
    const page = await cleanupApi.list(cleanup.next ?? undefined);
    cleanup.batches.push(...page.items);
    cleanup.next = page.next_cursor;
  } catch (e) {
    fail(e);
  }
}

/**
 * The choice a check was made with, read back from its own `since` and `limit`. A start time is a number
 * of days (whole days to now, so a 90-day window restores as 90). With none, a limit below the daemon's
 * cap (`limits.check_max`) is the newest N. "Newest 2000" and "All mail" send the same request, so a check
 * cannot tell them apart and they are equal by design: it comes back as "All mail".
 */
function choiceOf(c: cleanupApi.CleanupCheck): Pick<Scope, 'mode'> & Partial<Pick<Scope, 'newest' | 'days'>> {
  if (c.since !== null) return { mode: 'days', days: Math.max(1, Math.round((Date.now() / 1000 - c.since) / 86400)) };
  return c.limit < settings.value.limits.check_max ? { mode: 'newest', newest: c.limit } : { mode: 'all' };
}

// Without the list only Inbox is offered, so a failure here costs the Archive choice and nothing else.
async function loadFolders() {
  const id = cleanup.scope.accountId;
  cleanup.folders = [];
  if (!id) return;
  const found = await cleanupApi.folders(Number(id)).catch(() => []);
  if (id === cleanup.scope.accountId && cleanup.mailboxes.length === 1) cleanup.folders = found;
}

/** Neither a running check or sort, nor one on show, may have what it was made with changed: Discard it first. */
const locked = () => cleanup.phase === 'checking' || cleanup.phase === 'sorting' || cleanup.phase === 'ready' || cleanup.phase === 'stale';

/**
 * Picks the mailboxes to run over, in the order of the mailbox list. With one picked, the folder is its
 * own; with several, mail is taken from each Inbox, the one folder every mailbox has.
 */
export function setMailboxes(ids: string[]) {
  if (locked()) return;
  const order = accounts.list.map((a) => String(a.id));
  const rank = (id: string) => (order.includes(id) ? order.indexOf(id) : order.length);
  const picked = [...new Set(ids)].sort((a, b) => rank(a) - rank(b));
  const was = cleanup.scope.accountId;
  cleanup.mailboxes = picked;
  if (!picked.includes(was)) cleanup.scope.accountId = picked[0] ?? '';
  if (picked.length !== 1 || cleanup.scope.accountId !== was) cleanup.scope.folder = 'INBOX';
  if (picked.length === 1) loadFolders();
  else cleanup.folders = [];
  clearRun();
}

/** Picks the rules to check, or null for every rule. */
export function setRules(ids: number[] | null) {
  if (locked()) return;
  cleanup.ruleIds = ids;
  clearRun();
}

/** Changes the folder or range; `accountId` picks that one mailbox. Refused while a run is checking, sorting or on show. */
export function setScope(patch: Partial<Scope>) {
  if (locked()) return;
  const { accountId, ...rest } = patch;
  if (accountId !== undefined) setMailboxes([accountId]);
  Object.assign(cleanup.scope, rest);
  clearRun();
}

/** Puts a mailbox of the run on show. */
export function focus(accountId: string) {
  if (laneOf(accountId)) cleanup.scope.accountId = accountId;
}

/** What is wrong with the picked rules, in a sentence; empty when nothing is. */
export function ruleProblem() {
  const picked = pickedRules();
  return picked !== null && !picked.length ? 'Pick at least one rule.' : '';
}

/** The picked rules still switched on; null when every rule is picked. A rule deleted or switched off since is dropped. */
export function pickedRules() {
  const on = new Set(rules.list.filter((r) => r.enabled).map((r) => r.id));
  return cleanup.ruleIds === null ? null : cleanup.ruleIds.filter((id) => on.has(id));
}

/** Fetch every mailbox's current check and, if there are any, restore the run: its scope, picks and state. */
async function loadChecks() {
  // Ticks the user changed a moment ago must reach the daemon before it is asked for them.
  await pushSelections();
  const edits = new Map(cleanup.lanes.map((l) => [l.accountId, saver(l.accountId).edits]));
  // A lane the daemon tells of for the first time has had no tick since.
  const ticked = (id: string) => (edits.get(id) ?? saver(id).edits) !== saver(id).edits;
  let found: cleanupApi.CleanupCheck[];
  try {
    found = await cleanupApi.listChecks();
  } catch {
    return;
  }
  if (cleanup.phase === 'sorting') return;
  if (!found.length) {
    // A run being started has no checks to be told of yet, and one that is done shows its outcome.
    if (cleanup.phase !== 'done' && cleanup.phase !== 'checking') clearRun();
    return;
  }
  const first = found[0];
  cleanup.scope.folder = first.folder;
  Object.assign(cleanup.scope, choiceOf(first));
  cleanup.ruleIds = first.rule_ids;
  cleanup.mailboxes = found.map((c) => String(c.account_id));
  if (!cleanup.mailboxes.includes(cleanup.scope.accountId)) cleanup.scope.accountId = cleanup.mailboxes[0];
  if (cleanup.mailboxes.length === 1 && !cleanup.folders.length) loadFolders();
  cleanup.lanes = found.map((c) => {
    const lane = laneOf(c.account_id) ?? newLane(String(c.account_id));
    // A tick made while the answer was on its way is newer than what the daemon sent back.
    adopt(lane, c, ticked(lane.accountId));
    return lane;
  });
  refreshPhase();
}

/** Take a check (from a GET, so its rows are present when ready or stale) into its lane. */
function adopt(lane: Lane, c: cleanupApi.CleanupCheck, keepTicks = false) {
  lane.check = c;
  // The daemon keeps the unticked rows with the check, so a reload or a return shows the same ticks.
  if (!keepTicks) lane.excluded = new Set(c.exclude);
}

/** The phase the run's checks add up to: running while any is, stale if any is, ready if any can be sorted. */
function refreshPhase() {
  const states = cleanup.lanes.map((l) => l.check?.status);
  if (!states.length) return;
  if (states.includes('running') || states.includes(undefined)) {
    cleanup.phase = 'checking';
    startCheckPoll();
    return;
  }
  stopCheckPoll();
  cleanup.phase = states.includes('stale') ? 'stale' : states.includes('ready') ? 'ready' : 'failed';
}

/** Checks whose lane is on show can be sorted: ready, with a selected row. */
const sortable = () => cleanup.lanes.filter((l) => l.check?.status === 'ready' && selectedCount(l) > 0);

export async function check() {
  if (cleanup.phase === 'checking' || cleanup.phase === 'sorting' || scopeProblem(cleanup.scope) || ruleProblem() || !cleanup.mailboxes.length) return;
  const ids = cleanup.mailboxes;
  // One mailbox sends the original request shape; several are started together.
  const { account_id, ...range } = toRequest(cleanup.scope);
  const request: cleanupApi.CleanupCheckRequest = ids.length > 1 ? { account_ids: ids.map(Number), ...range } : { account_id, ...range };
  const picked = pickedRules();
  if (picked) request.rule_ids = picked;
  cleanup.phase = 'checking';
  dropSelectionSave();
  cleanup.lanes = ids.map(newLane);
  try {
    for (const c of await cleanupApi.startChecks(request)) {
      const lane = laneOf(c.account_id);
      if (lane) lane.check = c;
    }
    refreshPhase();
  } catch (e) {
    cleanup.lanes = [];
    cleanup.phase = 'idle';
    fail(e);
  }
}

function startCheckPoll() {
  if (checkTimer) return;
  // check.progress events move the numbers; the poll covers a stream that is closed or missed one.
  checkTimer = setInterval(checkTick, POLL_MS);
}

function stopCheckPoll() {
  clearInterval(checkTimer);
  checkTimer = undefined;
}

// A failed poll is not a failed check: the next poll or event tries again. The GET carries rows
// once ready or stale, so no second call is needed here.
async function checkTick() {
  // The checks this timer belonged to are gone (state reset, discarded, finished): stop asking.
  if (!cleanup.lanes.length || cleanup.phase !== 'checking') {
    stopCheckPoll();
    return;
  }
  const found = await cleanupApi.listChecks().catch(() => null);
  if (!found || cleanup.phase !== 'checking') return;
  takeChecks(found);
}

/** Take what the daemon says of the run's checks into their lanes, then work out the phase. */
function takeChecks(found: cleanupApi.CleanupCheck[]) {
  for (const c of found) {
    const lane = laneOf(c.account_id);
    if (!lane) continue;
    if (c.status === 'running') lane.check = c;
    else adopt(lane, c);
  }
  refreshPhase();
}

// The event carries no rows, so GET the checks to load them once one is ready or stale.
async function onCheckProgress(c: cleanupApi.CleanupCheck) {
  const lane = laneOf(c.account_id);
  if (!lane || cleanup.phase !== 'checking') return;
  if (c.status === 'running') {
    lane.check = c;
    return;
  }
  if (c.status === 'failed') {
    lane.check = c;
    refreshPhase();
    return;
  }
  const found = await cleanupApi.listChecks().catch(() => null);
  if (!found || cleanup.phase !== 'checking') return;
  takeChecks(found);
}

/** Every selectable row's index across all pages of a mailbox, or only those of the rule named by `rule`. */
function selectableIndices(lane: Lane | null, rule?: string): number[] {
  return (lane?.check?.rows ?? []).filter((r) => r.selectable && (!rule || r.rule_name === rule)).map((r) => r.index);
}

/** Selectable rows the user kept ticked: what Sort will act on. Of one mailbox, or of every mailbox of the run. */
export const selectableCount = (lane: Lane | null = focused()) => selectableIndices(lane).length;
export const selectedCount = (lane?: Lane | null): number =>
  lane === undefined ? cleanup.lanes.reduce((n, l) => n + (l.check?.status === 'ready' ? selectedCount(l) : 0), 0) : selectableIndices(lane).filter((i) => !lane!.excluded.has(i)).length;

/** One bar of the chart: a rule and where it sends mail, or what stays put. */
export interface ChartRow {
  name: string;
  count: number;
  kind: 'rule' | 'trash' | 'left' | 'review';
}

/**
 * Where a rule's actions send an email, in the words the Action column uses: the folder of a move, the
 * folder a trash goes to (`trashTo` when it is not the server's Trash), Archive, or `here` for actions
 * that leave it in its folder (keep, mark read).
 */
function destination(actions: cleanupApi.CleanupCheckRow['actions'], trashTo: string | undefined, here: string) {
  const a = actions.find((x) => x.type === 'move' || x.type === 'trash' || x.type === 'archive');
  if (!a) return { to: here, trash: false };
  if (a.type === 'move') return { to: a.folder || '[folder]', trash: false };
  if (a.type === 'trash') return { to: trashTo ?? 'Trash', trash: true };
  return { to: 'Archive', trash: false };
}

/**
 * What a check would do, one row per rule and destination, then what is left in the folder and what
 * waits in Needs review. It follows the ticks: an unticked row counts as left in the folder, so the
 * rule rows add up to what Sort acts on. The order comes from the check alone (rules biggest first,
 * trash after them), so a rule unticked to nothing keeps its place with 0.
 */
export function chartRows(rows: cleanupApi.CleanupCheckRow[], excluded: ReadonlySet<number>, trashTo?: string, here = 'Inbox'): ChartRow[] {
  const byRule = new Map<string, { row: ChartRow; all: number }>();
  let left = 0;
  let review = 0;
  for (const r of rows) {
    if (r.review) {
      review++;
      continue;
    }
    if (!r.selectable) {
      left++;
      continue;
    }
    const { to, trash } = destination(r.actions, trashTo, here);
    const name = (r.rule_name || 'A rule') + ' → ' + to;
    const g = byRule.get(name) ?? { row: { name, count: 0, kind: trash ? 'trash' : 'rule' }, all: 0 };
    byRule.set(name, g);
    g.all++;
    if (!excluded.has(r.index)) g.row.count++;
    else left++;
  }
  const ordered = [...byRule.values()].sort((a, b) => Number(a.row.kind === 'trash') - Number(b.row.kind === 'trash') || b.all - a.all || a.row.name.localeCompare(b.row.name));
  return [...ordered.map((g) => g.row), { name: 'Left in ' + here, count: left, kind: 'left' }, { name: 'Needs review', count: review, kind: 'review' }];
}

const emails = (n: number) => n.toLocaleString() + (n === 1 ? ' email' : ' emails');

/**
 * The chart's heading: how many emails, from which range of `here`. A check that covered only the newest
 * `limits.check_max` of a larger range says so.
 */
export function chartTitle(c: cleanupApi.CleanupCheck, here = 'Inbox') {
  const n = c.rows.length;
  const choice = choiceOf(c);
  const range = choice.mode === 'days' ? (choice.days === 1 ? ' from the last day' : ' from the last ' + choice.days!.toLocaleString() + ' days') : '';
  if (!n) return 'No emails in ' + here + range + ' to sort';
  if (c.limit >= settings.value.limits.check_max && c.matched > c.total)
    return 'The newest ' + n.toLocaleString() + ' of ' + emails(c.matched) + ' in ' + here + range + ' would be sorted like this';
  if (choice.mode === 'newest') return (n === 1 ? 'The newest email' : 'The newest ' + emails(n)) + ' in ' + here + ' would be sorted like this';
  if (choice.mode === 'all') return (n === 1 ? 'The one email' : 'All ' + emails(n)) + ' in ' + here + ' would be sorted like this';
  return emails(n) + ' in ' + here + range + ' would be sorted like this';
}

/**
 * Ticks every selectable row of the mailbox on show, or with `rule` only that rule's rows (the table's
 * rule filter); rows the filter hides keep their ticks.
 */
export function selectAll(rule?: string) {
  const lane = focused();
  if (!lane) return;
  const shown = new Set(selectableIndices(lane, rule));
  lane.excluded = new Set([...lane.excluded].filter((i) => !shown.has(i)));
  ticksChanged(lane);
}

/** Unticks every selectable row of the mailbox on show, or with `rule` only that rule's rows, as selectAll. */
export function selectNone(rule?: string) {
  const lane = focused();
  if (!lane) return;
  lane.excluded = new Set([...lane.excluded, ...selectableIndices(lane, rule)]);
  ticksChanged(lane);
}

export function toggleRow(index: number) {
  const lane = focused();
  const row = lane?.check?.rows.find((r) => r.index === index);
  if (!lane || !row?.selectable) return;
  const next = new Set(lane.excluded);
  if (next.has(index)) next.delete(index);
  else next.add(index);
  lane.excluded = next;
  ticksChanged(lane);
}

const SAVE_MS = 300;

// Saving the ticks is one request at a time per mailbox, always with the latest list.
interface Saver {
  timer: ReturnType<typeof setTimeout> | undefined;
  saving: boolean;
  dirty: boolean;
  flight: Promise<void>;
  /** Counts tick changes, so an answer that was asked for before one is known to be older. */
  edits: number;
}
const savers = new Map<string, Saver>();
const saver = (id: string): Saver => {
  let s = savers.get(id);
  if (!s) savers.set(id, (s = { timer: undefined, saving: false, dirty: false, flight: Promise.resolve(), edits: 0 }));
  return s;
};

/** A tick changed: save the latest list shortly, one request for a burst. */
function ticksChanged(lane: Lane) {
  const s = saver(lane.accountId);
  s.edits++;
  if (cleanup.phase !== 'ready') return;
  s.dirty = true;
  clearTimeout(s.timer);
  s.timer = setTimeout(() => pushSelection(lane.accountId), SAVE_MS);
}

/** Forget saves that have not gone out (of one mailbox, or all): the check they belonged to is gone or replaced. */
function dropSelectionSave(accountId?: string) {
  for (const [id, s] of savers) {
    if (accountId !== undefined && id !== accountId) continue;
    clearTimeout(s.timer);
    s.timer = undefined;
    s.dirty = false;
  }
}

/** Resolves once nothing is left to send for any mailbox. */
const pushSelections = () => Promise.all([...savers.keys()].map(pushSelection)).then(() => undefined);

/**
 * Send the unticked rows to the daemon, one request at a time and always the latest list, so a
 * slow earlier save can never land after a newer one. A failed save keeps the local ticks, says
 * why, and is tried again with the next change. Resolves once nothing is left to send.
 */
async function pushSelection(accountId: string) {
  const s = saver(accountId);
  clearTimeout(s.timer);
  s.timer = undefined;
  if (s.saving) return s.flight;
  if (!s.dirty) return;
  s.saving = true;
  s.flight = (async () => {
    try {
      while (s.dirty) {
        s.dirty = false;
        const lane = laneOf(accountId);
        const c = lane?.check;
        if (!lane || !c || cleanup.phase !== 'ready') return;
        const exclude = selectableIndices(lane).filter((i) => lane.excluded.has(i));
        try {
          await cleanupApi.saveSelection({ account_id: c.account_id, check_id: c.id, exclude });
        } catch (e) {
          // Not worth a word once the check it was for is gone (Sort, Discard, a new check).
          if (laneOf(accountId)?.check?.id === c.id) fail(e);
        }
      }
    } finally {
      s.saving = false;
    }
  })();
  return s.flight;
}

/**
 * Sorts every ready mailbox of the run that has a row ticked, as one batch each, started together.
 * Refused unless the user is looking at ready checks with at least one row ticked.
 */
export async function sort() {
  if (cleanup.phase !== 'ready') return false;
  const lanes = sortable();
  if (!lanes.length) return false;
  // Sort sends its own lists; a save of the same ticks must not chase the checks Sort is about to use up.
  dropSelectionSave();
  const runs = lanes.map((l) => ({ account_id: l.check!.account_id, check_id: l.check!.id, exclude: selectableIndices(l).filter((i) => l.excluded.has(i)) }));
  let batches: cleanupApi.Batch[];
  try {
    batches = await cleanupApi.runSort(runs);
  } catch (e) {
    for (const l of lanes) ticksChanged(l); // the checks live on, so their saved ticks still have to be up to date
    if (e instanceof ApiError && e.code === 'preview_stale') {
      cleanup.phase = 'stale';
      flash('The rules changed since this check. Run a new check, then sort.');
    } else {
      fail(e);
    }
    return false;
  }
  // The checks Sort used are gone; the others (nothing ticked, or failed) are done with too.
  const used = new Set(runs.map((r) => String(r.account_id)));
  for (const l of cleanup.lanes) if (!used.has(l.accountId)) cleanupApi.discardCheck(Number(l.accountId)).catch(() => undefined);
  cleanup.lanes = batches.map((b) => ({ ...newLane(String(b.account_id)), batch: b }));
  cleanup.scope.accountId = cleanup.lanes[0].accountId;
  cleanup.batches.unshift(...batches);
  dropSelectionSave();
  follow();
  for (const b of batches) progress(b);
  return true;
}

/** Throws every mailbox's check away. */
export async function discard() {
  try {
    await Promise.all(cleanup.lanes.map((l) => cleanupApi.discardCheck(Number(l.accountId))));
  } catch (e) {
    fail(e);
    return;
  }
  clearRun();
}

function follow() {
  stopCheckPoll();
  cleanup.phase = 'sorting';
  // batch.progress moves the bars; the poll covers a stream that is closed or missed an event.
  stopBatchPoll();
  batchTimer = setInterval(tick, POLL_MS);
}

function stopBatchPoll() {
  clearInterval(batchTimer);
  batchTimer = undefined;
}

// A failed poll is not a failed batch: the next poll or event tries again.
async function tick() {
  const going = cleanup.lanes.filter((l) => l.batch?.status === 'running');
  // The sort this timer belonged to is gone (state reset, finished): stop asking.
  if (!going.length || cleanup.phase !== 'sorting') {
    stopBatchPoll();
    return;
  }
  await Promise.all(
    going.map(async (l) => {
      const b = await cleanupApi.get(l.batch!.id).catch(() => null);
      if (b) progress(b);
    }),
  );
}

function progress(b: cleanupApi.Batch) {
  // Only a row still running moves, so a late event cannot reopen a finished one.
  cleanup.batches = cleanup.batches.map((x) => (x.id === b.id && x.status === 'running' ? b : x));
  if (cleanup.phase !== 'sorting') return;
  const lane = cleanup.lanes.find((l) => l.batch?.id === b.id);
  if (!lane) return;
  lane.batch = b;
  if (cleanup.lanes.some((l) => l.batch?.status === 'running')) return;
  stopBatchPoll();
  cleanup.phase = 'done';
  flash(runOutcome(cleanup.lanes.flatMap((l) => (l.batch ? [l.batch] : []))));
}

subscribe('batch.progress', progress);
subscribe('check.progress', onCheckProgress);

/** The tail that names emails passed over because they moved since the check; empty when none did. */
const skippedTail = (b: cleanupApi.Batch) => (b.skipped > 0 ? ', ' + b.skipped.toLocaleString() + ' skipped (no longer in the folder)' : '');

/** Emails a Sort actually acted on: `done` counts every row handled, those passed over because they moved included. */
export const acted = (b: cleanupApi.Batch) => Math.max(0, b.done - b.skipped);

/** How a finished sort went. Cleanup honours dry-run, and then nothing was moved. */
export const outcome = (b: cleanupApi.Batch) =>
  b.status === 'failed'
    ? 'Cleanup was cut short after ' + acted(b).toLocaleString() + ' emails' + skippedTail(b) + '. What it did can be undone as one batch below.'
    : b.actions.dry_run
      ? 'Dry run: ' + acted(b).toLocaleString() + ' emails checked, nothing moved' + skippedTail(b) + '.'
      : 'Cleanup done: ' + acted(b).toLocaleString() + ' emails sorted' + skippedTail(b) + '. Undo it as one batch below.';

/** How a finished run went, over one mailbox or several. */
export function runOutcome(bs: cleanupApi.Batch[]) {
  if (bs.length === 1) return outcome(bs[0]);
  const n = bs.reduce((sum, b) => sum + acted(b), 0).toLocaleString();
  const skipped = bs.reduce((sum, b) => sum + b.skipped, 0);
  const tail = skipped > 0 ? ', ' + skipped.toLocaleString() + ' skipped (no longer in the folder)' : '';
  const cut = bs.filter((b) => b.status === 'failed').length;
  if (cut) return `Cleanup was cut short on ${cut} of ${bs.length} mailboxes after ${n} emails${tail}. What it did can be undone below.`;
  if (bs.every((b) => b.actions.dry_run)) return `Dry run: ${n} emails checked in ${bs.length} mailboxes, nothing moved${tail}.`;
  return `Cleanup done: ${n} emails sorted in ${bs.length} mailboxes${tail}. Undo it per mailbox, or the whole run, below.`;
}

/**
 * What a past run covered, for its label in "Batches you can undo". A start time reads "since <date>". A
 * check that held more mail than its limit took only the newest `limit` of it, which is said ("newest 25
 * emails"), so "all time" is kept for a run that really saw the whole range. A run made before the daemon
 * recorded this (`limit` null) has nothing to go on: with no start time it is "all time", as it always was.
 */
export function coverage(b: cleanupApi.Batch) {
  const since = b.since === null ? '' : 'since ' + day(b.since);
  const capped = b.limit !== null && b.matched !== null && b.matched > b.limit;
  const newest = capped ? `newest ${b.limit!.toLocaleString()} ${b.limit === 1 ? 'email' : 'emails'}` : '';
  return [since, newest].filter(Boolean).join(' · ') || 'all time';
}

/** A past run as the history shows it: its batches, one per mailbox (several only for a manual run over several). */
export interface Run {
  /** The batches' shared `run_id`, or the id of the one batch of a run over one mailbox. */
  id: number;
  batches: cleanupApi.Batch[];
}

/**
 * Groups past batches into runs, in the order the history lists them (a run takes the place of its newest
 * batch), the batches of a run in the order they were started.
 */
export function runs(batches: cleanupApi.Batch[]): Run[] {
  const out: Run[] = [];
  const byRun = new Map<number, Run>();
  for (const b of batches) {
    const own = b.run_id === null ? undefined : byRun.get(b.run_id);
    if (own) {
      own.batches.push(b);
      continue;
    }
    const run: Run = { id: b.run_id ?? b.id, batches: [b] };
    if (b.run_id !== null) byRun.set(b.run_id, run);
    out.push(run);
  }
  for (const r of out) r.batches.sort((a, b) => a.id - b.id);
  return out;
}

// Mirrors the daemon's store.UndoDays (internal/store/retention.go): a batch created more
// than this many days ago is refused whole with 409 too_old.
export const UNDO_DAYS = 30;

/** Why a finished batch offers no Undo; empty when it does. `now` is in milliseconds. */
export const noUndo = (b: cleanupApi.Batch, now = Date.now()) =>
  now / 1000 - b.created_at > UNDO_DAYS * 86400
    ? 'Too old to undo'
    : !b.actions.done && !b.actions.failed && !b.actions.undone && b.actions.dry_run
      ? 'Dry run: nothing to undo'
      : '';

/** Whether a batch can be undone now: it has finished and nothing stops it. */
export const undoable = (b: cleanupApi.Batch) => (b.status === 'done' || b.status === 'failed') && !noUndo(b);

/**
 * The question asked before a batch undo. The emails are counted only when the batch has actions in
 * effect and every one is real (none dry-run, failed or already undone); otherwise how many move back is
 * known only once the daemon has done it, so the question names no number.
 */
export function undoQuestion(b: cleanupApi.Batch) {
  const n = acted(b);
  if (!n || !b.actions.done || b.actions.dry_run || b.actions.failed || b.actions.undone) return 'Put every email this batch moved back where it was?';
  return n === 1 ? 'Move 1 email back to where it was?' : 'Move ' + n.toLocaleString() + ' emails back to where they were?';
}

/** The question asked before undoing a whole run: every mailbox's batch that can be undone. */
export function undoRunQuestion(bs: cleanupApi.Batch[]) {
  const todo = bs.filter(undoable);
  const counted = todo.every((b) => acted(b) && b.actions.done && !b.actions.dry_run && !b.actions.failed && !b.actions.undone);
  const where = todo.length === 1 ? 'its mailbox' : `${todo.length} mailboxes`;
  if (!counted) return `Put every email this run moved back where it was, in ${where}?`;
  const n = todo.reduce((sum, b) => sum + acted(b), 0);
  return `Move ${n.toLocaleString()} ${n === 1 ? 'email' : 'emails'} back to where ${n === 1 ? 'it was' : 'they were'}, in ${where}?`;
}

/** Takes an undone batch back into the history, and into the run on show, which is over once all of it is undone. */
function undone(batch: cleanupApi.Batch) {
  cleanup.batches = cleanup.batches.map((x) => (x.id === batch.id ? batch : x));
  const lane = cleanup.lanes.find((l) => l.batch?.id === batch.id);
  if (!lane) return;
  lane.batch = batch;
  if (cleanup.phase === 'done' && cleanup.lanes.every((l) => l.batch?.status === 'undone')) clearRun();
}

export async function undo(b: cleanupApi.Batch) {
  try {
    const r = await cleanupApi.undo(b.id);
    // The batch is undone only when every action was; otherwise Undo stays on offer.
    undone(r.batch);
    flash(undoneText(r));
  } catch (e) {
    fail(e);
  }
}

/**
 * Undoes every batch of a run that can be, one mailbox after another, and says how it went as a whole. A
 * mailbox that fails does not stop the rest; the first failure is told after the others have been tried.
 */
export async function undoRun(bs: cleanupApi.Batch[]) {
  const total = { undone: 0, emails: 0, failed: 0 };
  let problem: unknown;
  for (const b of bs.filter(undoable)) {
    try {
      const r = await cleanupApi.undo(b.id);
      undone(r.batch);
      total.undone += r.undone;
      total.emails += r.emails;
      total.failed += r.failed;
    } catch (e) {
      problem ??= e;
    }
  }
  if (problem) fail(problem);
  else flash(undoneText(total));
}

/** The toast after a batch undo: the emails it really put back, never the batch's ticked rows (some may have been skipped or only recorded in dry-run). */
export function undoneText(r: Pick<cleanupApi.UndoResult, 'undone' | 'emails' | 'failed'>) {
  if (r.failed) return r.undone.toLocaleString() + ' actions undone; ' + r.failed.toLocaleString() + ' could not be';
  if (!r.emails) return 'Nothing to undo';
  return r.emails === 1 ? '1 email moved back where it was' : r.emails.toLocaleString() + ' emails moved back where they were';
}
