import * as cleanupApi from '../api/cleanup';
import { flash } from './toast.svelte';

// idle → previewed → running → done. Changing the scope goes back to idle from anywhere but
// running, so a run always matches the preview the user saw.
type Phase = 'idle' | 'previewed' | 'running' | 'done';

export const cleanup = $state<{
  phase: Phase;
  scope: cleanupApi.Scope;
  preview: cleanupApi.Preview | null;
  /** The run in progress, or the one that just finished. */
  batch: cleanupApi.Batch | null;
  batches: cleanupApi.Batch[];
}>({
  phase: 'idle',
  scope: { accountId: '', folder: 'INBOX', range: '90' },
  preview: null,
  batch: null,
  batches: [],
});

let timer: ReturnType<typeof setInterval> | undefined;

export async function load() {
  cleanup.batches = await cleanupApi.list();
}

export function setScope(patch: Partial<cleanupApi.Scope>) {
  if (cleanup.phase === 'running') return;
  Object.assign(cleanup.scope, patch);
  cleanup.preview = null;
  cleanup.phase = 'idle';
}

export async function preview() {
  if (cleanup.phase === 'running') return;
  cleanup.preview = await cleanupApi.preview($state.snapshot(cleanup.scope));
  cleanup.phase = 'previewed';
}

/** Refused unless the user is looking at a preview of this scope. */
export async function run(tickMs = 160) {
  if (cleanup.phase !== 'previewed') return false;
  cleanup.phase = 'running';
  cleanup.batch = await cleanupApi.run($state.snapshot(cleanup.scope));
  // Polls until the batch.progress event exists (docs/frontend-plan.md → SSE events to state).
  timer = setInterval(tick, tickMs);
  return true;
}

async function tick() {
  const b = await cleanupApi.get(cleanup.batch!.id);
  if (cleanup.phase !== 'running') return;
  cleanup.batch = b;
  if (b.status === 'running') return;
  clearInterval(timer);
  cleanup.phase = 'done';
  cleanup.preview = null;
  cleanup.batches = [b, ...cleanup.batches];
  flash('Cleanup done: ' + b.total.toLocaleString() + ' emails sorted. Undo it as one batch below.');
}

export async function undo(id: string) {
  const b = await cleanupApi.undo(id);
  cleanup.batches = cleanup.batches.map((x) => (x.id === id ? b : x));
  if (cleanup.phase === 'done' && cleanup.batch?.id === id) cleanup.phase = 'idle';
  flash(b.total.toLocaleString() + ' emails moved back where they were');
}
