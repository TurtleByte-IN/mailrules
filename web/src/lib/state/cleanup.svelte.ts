import { ApiError } from '../api/client';
import * as cleanupApi from '../api/cleanup';
import { subscribe } from '../api/events';
import { TEST_LIMIT } from '../api/rules';
import { day } from '../format';
import { flash } from './toast.svelte';

// idle → checking (a real check is running) → ready/stale (rows shown) or failed.
// From ready, Sort moves to sorting → done. Changing the scope goes back to idle unless a check
// or a sort is running, so what the user sorts always matches the check they are looking at.
type Phase = 'idle' | 'checking' | 'ready' | 'stale' | 'failed' | 'sorting' | 'done';

/** The most emails one check covers: the daemon's own bound, the same number the rule tester allows. */
export const CHECK_MAX = TEST_LIMIT.max;

/** What the scope controls hold; toRequest turns it into the contract's CleanupCheckRequest. */
export interface Scope {
  accountId: string;
  /** The server's own folder name. */
  folder: string;
  /** The newest N emails, the emails from the last N days, or all mail. Every one is capped at CHECK_MAX. */
  mode: 'newest' | 'days' | 'all';
  /** The "Newest emails" box; null is an empty box. */
  newest: number | null;
  /** The "From the last days" box; null is an empty box. */
  days: number | null;
}

export const cleanup = $state<{
  phase: Phase;
  scope: Scope;
  /** Folders of the chosen mailbox. */
  folders: cleanupApi.Folder[];
  /** The account's current check: its progress while running, its rows when ready or stale. */
  check: cleanupApi.CleanupCheck | null;
  /** Indices of selectable rows the user unticked; empty = every selectable row is sorted. */
  excluded: Set<number>;
  /** The sort being followed, or the last one that finished here. */
  batch: cleanupApi.Batch | null;
  /** Past runs, newest first. */
  batches: cleanupApi.Batch[];
  /** Cursor of the next page of past runs; null at the end. */
  next: string | null;
  status: 'loading' | 'ready' | 'error';
  error: string;
}>({
  phase: 'idle',
  scope: { accountId: '', folder: 'INBOX', mode: 'newest', newest: TEST_LIMIT.default, days: 90 },
  folders: [],
  check: null,
  excluded: new Set(),
  batch: null,
  batches: [],
  next: null,
  status: 'loading',
  error: '',
});

const POLL_MS = 2000;

let batchTimer: ReturnType<typeof setInterval> | undefined;
let checkTimer: ReturnType<typeof setInterval> | undefined;

const fail = (e: unknown) => flash((e as Error).message);

export async function load() {
  try {
    const page = await cleanupApi.list();
    cleanup.batches = page.items;
    cleanup.next = page.next_cursor;
    cleanup.status = 'ready';
    // A sort that was going before the page loaded is followed like one started here.
    const going = page.items.find((b) => b.status === 'running');
    if (going && cleanup.phase !== 'sorting') follow(going);
  } catch (e) {
    cleanup.status = 'error';
    cleanup.error = (e as Error).message;
  }
  // Restore the check for the mailbox when returning to the page; the first-time mailbox pick
  // goes through setScope instead, which does the same.
  await loadCheck();
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

/** What is wrong with the chosen number, in a sentence; empty when nothing is, and for "All mail", which has no box. */
export function scopeProblem(s: Scope) {
  if (s.mode === 'newest') return s.newest !== null && Number.isInteger(s.newest) && s.newest >= 1 && s.newest <= CHECK_MAX ? '' : `The limit must be between 1 and ${CHECK_MAX}.`;
  if (s.mode === 'days') return s.days !== null && Number.isInteger(s.days) && s.days >= 1 ? '' : 'Give a whole number of days, 1 or more.';
  return '';
}

// Every check covers at most the newest CHECK_MAX emails of its range, so the limit is always sent.
// A start before the epoch is the epoch: a huge number of days means all mail.
function toRequest(s: Scope): cleanupApi.CleanupCheckRequest {
  const account_id = Number(s.accountId);
  if (s.mode === 'newest') return { account_id, folder: s.folder, since: null, limit: Math.min(s.newest!, CHECK_MAX) };
  if (s.mode === 'days') return { account_id, folder: s.folder, since: Math.max(0, Math.floor(Date.now() / 1000) - s.days! * 86400), limit: CHECK_MAX };
  return { account_id, folder: s.folder, since: null, limit: CHECK_MAX };
}

/**
 * The choice a check was made with, read back from its own `since` and `limit`. A start time is a number
 * of days (whole days to now, so a 90-day window restores as 90). With none, a limit below the cap is the
 * newest N. "Newest 2000" and "All mail" send the same request, so a check cannot tell them apart and
 * they are equal by design: it comes back as "All mail".
 */
function choiceOf(c: cleanupApi.CleanupCheck): Pick<Scope, 'mode'> & Partial<Pick<Scope, 'newest' | 'days'>> {
  if (c.since !== null) return { mode: 'days', days: Math.max(1, Math.round((Date.now() / 1000 - c.since) / 86400)) };
  return c.limit < CHECK_MAX ? { mode: 'newest', newest: c.limit } : { mode: 'all' };
}

// Without the list only Inbox is offered, so a failure here costs the Archive choice and nothing else.
async function loadFolders() {
  const id = cleanup.scope.accountId;
  cleanup.folders = [];
  const found = await cleanupApi.folders(Number(id)).catch(() => []);
  if (id === cleanup.scope.accountId) cleanup.folders = found;
}

export function setScope(patch: Partial<Scope>) {
  // Neither a running check nor a running sort may have its scope changed under it, and the
  // scope of a check on show is that check's own: Discard it to choose another.
  if (cleanup.phase === 'checking' || cleanup.phase === 'sorting') return;
  const other = patch.accountId !== undefined && patch.accountId !== cleanup.scope.accountId;
  if (!other && (cleanup.phase === 'ready' || cleanup.phase === 'stale')) return;
  Object.assign(cleanup.scope, patch);
  if (patch.accountId !== undefined) {
    cleanup.scope.folder = 'INBOX';
    loadFolders();
  }
  cleanup.check = null;
  dropSelectionSave();
  cleanup.excluded = new Set();
  cleanup.phase = 'idle';
  // Another mailbox may have a check of its own to come back to; a change of folder or number may not.
  if (patch.accountId !== undefined) loadCheck();
}

/** Fetch the account's current check and, if there is one, restore the scope and show its state. */
async function loadCheck() {
  const id = cleanup.scope.accountId;
  if (!id) {
    cleanup.check = null;
    return;
  }
  // Ticks the user changed a moment ago must reach the daemon before it is asked for them.
  await pushSelection();
  const ticks = tickEdits;
  let c: cleanupApi.CleanupCheck | null;
  try {
    c = await cleanupApi.getCheck(Number(id));
  } catch {
    return;
  }
  if (id !== cleanup.scope.accountId || cleanup.phase === 'sorting') return;
  if (!c) {
    cleanup.check = null;
    dropSelectionSave();
    cleanup.excluded = new Set();
    if (cleanup.phase !== 'done') cleanup.phase = 'idle';
    return;
  }
  cleanup.scope.folder = c.folder;
  Object.assign(cleanup.scope, choiceOf(c));
  if (!cleanup.folders.length) loadFolders();
  // A tick made while the answer was on its way is newer than what the daemon sent back.
  adopt(c, ticks !== tickEdits);
}

/** Take a check (from a GET, so its rows are present when ready or stale) and show it. */
function adopt(c: cleanupApi.CleanupCheck, keepTicks = false) {
  cleanup.check = c;
  // The daemon keeps the unticked rows with the check, so a reload or a return shows the same ticks.
  if (!keepTicks) cleanup.excluded = new Set(c.exclude);
  if (c.status === 'running') {
    cleanup.phase = 'checking';
    startCheckPoll();
  } else {
    stopCheckPoll();
    cleanup.phase = c.status === 'ready' ? 'ready' : c.status === 'stale' ? 'stale' : 'failed';
  }
}

export async function check() {
  if (cleanup.phase === 'checking' || cleanup.phase === 'sorting' || scopeProblem(cleanup.scope)) return;
  const request = toRequest(cleanup.scope);
  cleanup.phase = 'checking';
  dropSelectionSave();
  cleanup.excluded = new Set();
  try {
    const c = await cleanupApi.startCheck(request);
    cleanup.check = c;
    startCheckPoll();
  } catch (e) {
    cleanup.phase = 'idle';
    fail(e);
  }
}

function startCheckPoll() {
  stopCheckPoll();
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
  const id = cleanup.scope.accountId;
  if (!id || cleanup.phase !== 'checking') return;
  const c = await cleanupApi.getCheck(Number(id)).catch(() => null);
  if (!c || cleanup.phase !== 'checking' || String(c.account_id) !== cleanup.scope.accountId) return;
  if (c.status === 'running') {
    cleanup.check = c;
    return;
  }
  adopt(c);
}

// The event carries no rows, so GET the check to load them once it is ready or stale.
async function onCheckProgress(c: cleanupApi.CleanupCheck) {
  if (String(c.account_id) !== cleanup.scope.accountId || cleanup.phase !== 'checking') return;
  if (c.status === 'running') {
    cleanup.check = c;
    return;
  }
  if (c.status === 'failed') {
    stopCheckPoll();
    cleanup.check = c;
    cleanup.phase = 'failed';
    return;
  }
  const full = await cleanupApi.getCheck(c.account_id).catch(() => null);
  if (!full || String(full.account_id) !== cleanup.scope.accountId || cleanup.phase !== 'checking') return;
  adopt(full);
}

/** Every selectable row's index, across all pages and filters. */
function selectableIndices(): number[] {
  return (cleanup.check?.rows ?? []).filter((r) => r.selectable).map((r) => r.index);
}

/** Selectable rows the user kept ticked: what Sort will act on. */
export const selectableCount = () => selectableIndices().length;
export const selectedCount = () => selectableIndices().filter((i) => !cleanup.excluded.has(i)).length;

export function selectAll() {
  cleanup.excluded = new Set();
  ticksChanged();
}

export function selectNone() {
  cleanup.excluded = new Set(selectableIndices());
  ticksChanged();
}

export function toggleRow(index: number) {
  const row = cleanup.check?.rows.find((r) => r.index === index);
  if (!row?.selectable) return;
  const next = new Set(cleanup.excluded);
  if (next.has(index)) next.delete(index);
  else next.add(index);
  cleanup.excluded = next;
  ticksChanged();
}

const SAVE_MS = 300;

let saveTimer: ReturnType<typeof setTimeout> | undefined;
let saving = false;
let dirty = false;
let flight: Promise<void> = Promise.resolve();
// Counts tick changes, so an answer that was asked for before one is known to be older.
let tickEdits = 0;

/** A tick changed: save the latest list shortly, one request for a burst. */
function ticksChanged() {
  tickEdits++;
  if (cleanup.phase !== 'ready') return;
  dirty = true;
  clearTimeout(saveTimer);
  saveTimer = setTimeout(pushSelection, SAVE_MS);
}

/** Forget a save that has not gone out: the check it belonged to is gone or replaced. */
function dropSelectionSave() {
  clearTimeout(saveTimer);
  saveTimer = undefined;
  dirty = false;
}

/**
 * Send the unticked rows to the daemon, one request at a time and always the latest list, so a
 * slow earlier save can never land after a newer one. A failed save keeps the local ticks, says
 * why, and is tried again with the next change. Resolves once nothing is left to send.
 */
async function pushSelection() {
  clearTimeout(saveTimer);
  saveTimer = undefined;
  if (saving) return flight;
  if (!dirty) return;
  saving = true;
  flight = (async () => {
    try {
      while (dirty) {
        dirty = false;
        const c = cleanup.check;
        if (!c || cleanup.phase !== 'ready') return;
        const exclude = selectableIndices().filter((i) => cleanup.excluded.has(i));
        try {
          await cleanupApi.saveSelection({ account_id: c.account_id, check_id: c.id, exclude });
        } catch (e) {
          // Not worth a word once the check it was for is gone (Sort, Discard, a new check).
          if (cleanup.check?.id === c.id) fail(e);
        }
      }
    } finally {
      saving = false;
    }
  })();
  return flight;
}

/** Refused unless the user is looking at a ready check with at least one row ticked. */
export async function sort() {
  if (cleanup.phase !== 'ready' || !cleanup.check || selectedCount() === 0) return false;
  // Sort sends its own list; a save of the same ticks must not chase the check Sort is about to use up.
  dropSelectionSave();
  const exclude = selectableIndices().filter((i) => cleanup.excluded.has(i));
  const request: cleanupApi.CleanupRunRequest = { account_id: cleanup.check.account_id, check_id: cleanup.check.id, exclude };
  let b: cleanupApi.Batch;
  try {
    b = await cleanupApi.runSort(request);
  } catch (e) {
    ticksChanged(); // the check lives on, so its saved ticks still have to be up to date
    if (e instanceof ApiError && e.code === 'preview_stale') {
      cleanup.phase = 'stale';
      flash('The rules changed since this check. Run a new check, then sort.');
    } else {
      fail(e);
    }
    return false;
  }
  cleanup.batches.unshift(b);
  cleanup.check = null;
  dropSelectionSave();
  cleanup.excluded = new Set();
  follow(b);
  progress(b);
  return true;
}

export async function discard() {
  const id = cleanup.scope.accountId;
  try {
    await cleanupApi.discardCheck(Number(id));
  } catch (e) {
    fail(e);
    return;
  }
  stopCheckPoll();
  cleanup.check = null;
  dropSelectionSave();
  cleanup.excluded = new Set();
  cleanup.phase = 'idle';
}

function follow(b: cleanupApi.Batch) {
  stopCheckPoll();
  cleanup.phase = 'sorting';
  cleanup.batch = b;
  // batch.progress moves the bar; the poll covers a stream that is closed or missed an event.
  batchTimer = setInterval(tick, POLL_MS);
}

// A failed poll is not a failed batch: the next poll or event tries again.
async function tick() {
  const b = await cleanupApi.get(cleanup.batch!.id).catch(() => null);
  if (b) progress(b);
}

function progress(b: cleanupApi.Batch) {
  // Only a row still running moves, so a late event cannot reopen a finished one.
  cleanup.batches = cleanup.batches.map((x) => (x.id === b.id && x.status === 'running' ? b : x));
  if (cleanup.phase !== 'sorting' || b.id !== cleanup.batch?.id) return;
  cleanup.batch = b;
  if (b.status === 'running') return;
  clearInterval(batchTimer);
  cleanup.phase = 'done';
  flash(outcome(b));
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

export async function undo(b: cleanupApi.Batch) {
  try {
    const r = await cleanupApi.undo(b.id);
    // The batch is undone only when every action was; otherwise Undo stays on offer.
    cleanup.batches = cleanup.batches.map((x) => (x.id === b.id ? r.batch : x));
    if (cleanup.batch?.id === b.id) {
      cleanup.batch = r.batch;
      if (cleanup.phase === 'done' && r.batch.status === 'undone') cleanup.phase = 'idle';
    }
    const n = (r.batch.total ?? 0).toLocaleString();
    flash(r.failed ? r.undone.toLocaleString() + ' actions undone; ' + r.failed.toLocaleString() + ' could not be' : n + ' emails moved back where they were');
  } catch (e) {
    fail(e);
  }
}
