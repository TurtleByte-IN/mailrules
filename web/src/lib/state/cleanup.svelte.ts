import * as cleanupApi from '../api/cleanup';
import { notBuilt } from '../api/client';
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
  /** Set once the daemon answers 501 for preview or run. */
  notBuilt: boolean;
  scope: Scope;
  /** Folders of the chosen mailbox. */
  folders: cleanupApi.Folder[];
  preview: cleanupApi.Preview | null;
  /** What the showing preview was counted for; run sends the same. */
  request: cleanupApi.CleanupRequest | null;
  /** The run started in this session: in progress, finished or undone. */
  batch: cleanupApi.Batch | null;
  batchScope: Scope | null;
}>({
  phase: 'idle',
  notBuilt: false,
  scope: { accountId: '', folder: 'INBOX', range: '90' },
  folders: [],
  preview: null,
  request: null,
  batch: null,
  batchScope: null,
});

let timer: ReturnType<typeof setInterval> | undefined;
// Counts scope changes and runs, so a preview that answers after one is dropped.
let edits = 0;

function fail(e: unknown) {
  if (notBuilt(e)) cleanup.notBuilt = true;
  else flash((e as Error).message);
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
export async function run(pollMs = 2000) {
  if (cleanup.phase !== 'previewed' || !cleanup.request) return false;
  edits++;
  cleanup.phase = 'running';
  try {
    cleanup.batch = await cleanupApi.run($state.snapshot(cleanup.request));
  } catch (e) {
    cleanup.phase = 'previewed';
    fail(e);
    return false;
  }
  cleanup.batchScope = $state.snapshot(cleanup.scope);
  // batch.progress moves the bar; the poll covers a stream that is closed or missed an event.
  timer = setInterval(tick, pollMs);
  progress(cleanup.batch);
  return true;
}

// A failed poll is not a failed batch: the next poll or event tries again.
async function tick() {
  const b = await cleanupApi.get(cleanup.batch!.id).catch(() => null);
  if (b) progress(b);
}

function progress(b: cleanupApi.Batch) {
  if (cleanup.phase !== 'running' || b.id !== cleanup.batch?.id) return;
  cleanup.batch = b;
  if (b.status === 'running') return;
  clearInterval(timer);
  cleanup.phase = 'done';
  cleanup.preview = null;
  cleanup.request = null;
  flash(outcome(b));
}

subscribe('batch.progress', (b) => progress(b as cleanupApi.Batch));

/** How a finished run went. Cleanup honours dry-run, and then nothing was moved. */
export const outcome = (b: cleanupApi.Batch) =>
  b.status === 'failed'
    ? 'Cleanup failed after ' + b.done.toLocaleString() + ' emails. Undo it as one batch below.'
    : b.actions.dry_run
      ? 'Dry run: ' + b.done.toLocaleString() + ' emails checked, nothing moved.'
      : 'Cleanup done: ' + b.done.toLocaleString() + ' emails sorted. Undo it as one batch below.';

export async function undo() {
  if (!cleanup.batch) return;
  try {
    const r = await cleanupApi.undo(cleanup.batch.id);
    cleanup.batch = r.batch;
    // The batch is undone only when every action was; otherwise Undo stays on offer.
    if (cleanup.phase === 'done' && r.batch.status === 'undone') cleanup.phase = 'idle';
    flash(r.undone.toLocaleString() + ' emails moved back where they were' + (r.failed ? '; ' + r.failed.toLocaleString() + ' could not be' : ''));
  } catch (e) {
    fail(e);
  }
}
