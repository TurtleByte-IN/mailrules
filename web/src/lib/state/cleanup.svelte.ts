import * as cleanupApi from '../api/cleanup';
import { subscribe } from '../api/events';
import { flash } from './toast.svelte';

// idle → previewed → running → done. Changing the scope goes back to idle from anywhere but
// running, so a run always matches the preview the user saw.
type Phase = 'idle' | 'previewed' | 'running' | 'done';

/** What the scope controls hold; toRequest turns it into the contract's CleanupRequest. */
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
  preview: cleanupApi.Preview | null;
  /** What the showing preview was counted for; run sends the same. */
  request: cleanupApi.CleanupRequest | null;
  /** The run being followed, or the last one that finished here. */
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
  preview: null,
  request: null,
  batch: null,
  batches: [],
  next: null,
  status: 'loading',
  error: '',
});

const POLL_MS = 2000;

let timer: ReturnType<typeof setInterval> | undefined;
// Counts scope changes and runs, so a preview that answers after one is dropped.
let edits = 0;

const fail = (e: unknown) => flash((e as Error).message);

export async function load() {
  try {
    const page = await cleanupApi.list();
    cleanup.batches = page.items;
    cleanup.next = page.next_cursor;
    cleanup.status = 'ready';
    // A run that was going before the page loaded is followed like one started here.
    // ponytail: one run is followed; a second mailbox's run moves only its row, by events.
    const going = page.items.find((b) => b.status === 'running');
    if (going && cleanup.phase !== 'running') follow(going);
  } catch (e) {
    cleanup.status = 'error';
    cleanup.error = (e as Error).message;
  }
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

const toRequest = (s: Scope): cleanupApi.CleanupRequest => ({
  account_id: Number(s.accountId),
  folder: s.folder,
  since: s.range === 'all' ? null : Math.floor(Date.now() / 1000) - Number(s.range) * 86400,
});

// Without the list only Inbox is offered, so a failure here costs the Archive choice and nothing else.
async function loadFolders() {
  const id = cleanup.scope.accountId;
  cleanup.folders = [];
  const found = await cleanupApi.folders(Number(id)).catch(() => []);
  if (id === cleanup.scope.accountId) cleanup.folders = found;
}

export function setScope(patch: Partial<Scope>) {
  if (cleanup.phase === 'running') return;
  edits++;
  Object.assign(cleanup.scope, patch);
  if (patch.accountId !== undefined) {
    cleanup.scope.folder = 'INBOX';
    loadFolders();
  }
  cleanup.preview = null;
  cleanup.request = null;
  cleanup.phase = 'idle';
}

export async function preview() {
  if (cleanup.phase === 'running') return;
  const at = edits;
  const request = toRequest(cleanup.scope);
  try {
    const p = await cleanupApi.preview(request);
    if (at !== edits) return;
    cleanup.preview = p;
    cleanup.request = request;
    cleanup.phase = 'previewed';
  } catch (e) {
    fail(e);
  }
}

/** Refused unless the user is looking at a preview of this scope. */
export async function run() {
  if (cleanup.phase !== 'previewed' || !cleanup.request) return false;
  edits++;
  cleanup.phase = 'running';
  let b: cleanupApi.Batch;
  try {
    b = await cleanupApi.run($state.snapshot(cleanup.request));
  } catch (e) {
    cleanup.phase = 'previewed';
    fail(e);
    return false;
  }
  cleanup.batches.unshift(b);
  follow(b);
  progress(b);
  return true;
}

function follow(b: cleanupApi.Batch) {
  // A preview still on its way must not land on a run.
  edits++;
  cleanup.phase = 'running';
  cleanup.batch = b;
  // batch.progress moves the bar; the poll covers a stream that is closed or missed an event.
  timer = setInterval(tick, POLL_MS);
}

// A failed poll is not a failed batch: the next poll or event tries again.
async function tick() {
  const b = await cleanupApi.get(cleanup.batch!.id).catch(() => null);
  if (b) progress(b);
}

function progress(b: cleanupApi.Batch) {
  // Only a row still running moves, so a late event cannot reopen a finished one.
  cleanup.batches = cleanup.batches.map((x) => (x.id === b.id && x.status === 'running' ? b : x));
  if (cleanup.phase !== 'running' || b.id !== cleanup.batch?.id) return;
  cleanup.batch = b;
  if (b.status === 'running') return;
  clearInterval(timer);
  cleanup.phase = 'done';
  cleanup.preview = null;
  cleanup.request = null;
  flash(outcome(b));
}

subscribe('batch.progress', progress);

/** How a finished run went. Cleanup honours dry-run, and then nothing was moved. */
export const outcome = (b: cleanupApi.Batch) =>
  b.status === 'failed'
    ? 'Cleanup was cut short after ' + b.done.toLocaleString() + ' emails. What it did can be undone as one batch below.'
    : b.actions.dry_run
      ? 'Dry run: ' + b.done.toLocaleString() + ' emails checked, nothing moved.'
      : 'Cleanup done: ' + b.done.toLocaleString() + ' emails sorted. Undo it as one batch below.';

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
