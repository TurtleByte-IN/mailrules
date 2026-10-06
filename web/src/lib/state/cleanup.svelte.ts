import { ApiError } from '../api/client';
import * as cleanupApi from '../api/cleanup';
import { subscribe } from '../api/events';
import { flash } from './toast.svelte';

// idle → checking (a real check is running) → ready/stale (rows shown) or failed.
// From ready, Sort moves to sorting → done. Changing the scope goes back to idle unless a check
// or a sort is running, so what the user sorts always matches the check they are looking at.
type Phase = 'idle' | 'checking' | 'ready' | 'stale' | 'failed' | 'sorting' | 'done';

/** What the scope controls hold; toRequest turns it into the contract's CleanupCheckRequest. */
export interface Scope {
  accountId: string;
  /** The server's own folder name. */
  folder: string;
  /** Days back, or all time. */
  range: '30' | '90' | '365' | 'all';
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
  scope: { accountId: '', folder: 'INBOX', range: '90' },
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

const toRequest = (s: Scope): cleanupApi.CleanupCheckRequest => ({
  account_id: Number(s.accountId),
  folder: s.folder,
  since: s.range === 'all' ? null : Math.floor(Date.now() / 1000) - Number(s.range) * 86400,
});

/** The range whose window a check's `since` sits closest to; `all` for no limit. */
function rangeOf(since: number | null): Scope['range'] {
  if (since === null) return 'all';
  const days = (Date.now() / 1000 - since) / 86400;
  const opts: Scope['range'][] = ['30', '90', '365'];
  return opts.reduce((best, r) => (Math.abs(Number(r) - days) < Math.abs(Number(best) - days) ? r : best), '30');
}

// Without the list only Inbox is offered, so a failure here costs the Archive choice and nothing else.
async function loadFolders() {
  const id = cleanup.scope.accountId;
  cleanup.folders = [];
  const found = await cleanupApi.folders(Number(id)).catch(() => []);
  if (id === cleanup.scope.accountId) cleanup.folders = found;
}

export function setScope(patch: Partial<Scope>) {
  // Neither a running check nor a running sort may have its scope changed under it.
  if (cleanup.phase === 'checking' || cleanup.phase === 'sorting') return;
  Object.assign(cleanup.scope, patch);
  if (patch.accountId !== undefined) {
    cleanup.scope.folder = 'INBOX';
    loadFolders();
  }
  cleanup.check = null;
  cleanup.excluded = new Set();
  cleanup.phase = 'idle';
  loadCheck();
}

/** Fetch the account's current check and, if there is one, restore the scope and show its state. */
async function loadCheck() {
  const id = cleanup.scope.accountId;
  if (!id) {
    cleanup.check = null;
    return;
  }
  let c: cleanupApi.CleanupCheck | null;
  try {
    c = await cleanupApi.getCheck(Number(id));
  } catch {
    return;
  }
  if (id !== cleanup.scope.accountId || cleanup.phase === 'sorting') return;
  if (!c) {
    cleanup.check = null;
    cleanup.excluded = new Set();
    if (cleanup.phase !== 'done') cleanup.phase = 'idle';
    return;
  }
  cleanup.scope.folder = c.folder;
  cleanup.scope.range = rangeOf(c.since);
  if (!cleanup.folders.length) loadFolders();
  adopt(c);
}

/** Take a check (from a GET, so its rows are present when ready or stale) and show it. */
function adopt(c: cleanupApi.CleanupCheck) {
  cleanup.check = c;
  cleanup.excluded = new Set();
  if (c.status === 'running') {
    cleanup.phase = 'checking';
    startCheckPoll();
  } else {
    stopCheckPoll();
    cleanup.phase = c.status === 'ready' ? 'ready' : c.status === 'stale' ? 'stale' : 'failed';
  }
}

export async function check() {
  if (cleanup.phase === 'checking' || cleanup.phase === 'sorting') return;
  const request = toRequest(cleanup.scope);
  cleanup.phase = 'checking';
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
}

export function selectNone() {
  cleanup.excluded = new Set(selectableIndices());
}

export function toggleRow(index: number) {
  const row = cleanup.check?.rows.find((r) => r.index === index);
  if (!row?.selectable) return;
  const next = new Set(cleanup.excluded);
  if (next.has(index)) next.delete(index);
  else next.add(index);
  cleanup.excluded = next;
}

/** Refused unless the user is looking at a ready check with at least one row ticked. */
export async function sort() {
  if (cleanup.phase !== 'ready' || !cleanup.check || selectedCount() === 0) return false;
  const exclude = selectableIndices().filter((i) => cleanup.excluded.has(i));
  const request: cleanupApi.CleanupRunRequest = { account_id: cleanup.check.account_id, check_id: cleanup.check.id, exclude };
  let b: cleanupApi.Batch;
  try {
    b = await cleanupApi.runSort(request);
  } catch (e) {
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

/** How a finished sort went. Cleanup honours dry-run, and then nothing was moved. */
export const outcome = (b: cleanupApi.Batch) =>
  b.status === 'failed'
    ? 'Cleanup was cut short after ' + b.done.toLocaleString() + ' emails. What it did can be undone as one batch below.'
    : b.actions.dry_run
      ? 'Dry run: ' + b.done.toLocaleString() + ' emails checked, nothing moved' + skippedTail(b) + '.'
      : 'Cleanup done: ' + b.done.toLocaleString() + ' emails sorted' + skippedTail(b) + '. Undo it as one batch below.';

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
