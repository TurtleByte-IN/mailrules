import { afterEach, beforeEach, expect, it, vi } from 'vitest';

// The demo API keeps its data in the module, so every test starts from a fresh copy.
let m: typeof import('./cleanup.svelte');
let toast: { text: string };

beforeEach(async () => {
  vi.useFakeTimers();
  vi.resetModules();
  m = await import('./cleanup.svelte');
  toast = (await import('./toast.svelte')).toast;
  m.setScope({ accountId: 'acc1', range: '30' });
});
afterEach(() => vi.useRealTimers());

// 7% a tick: fifteen ticks finish a batch.
const finish = () => vi.advanceTimersByTimeAsync(160 * 15);

it('run is refused before a preview', async () => {
  expect(await m.run()).toBe(false);
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.batch).toBeNull();
  expect(vi.getTimerCount()).toBe(0);
});

it.each([
  [{ range: '30' }, 412],
  [{ range: '90' }, 1236],
  [{ range: '30', folder: 'Archive' }, 288],
] as const)('idle → previewed: %j previews %i emails', async (scope, total) => {
  m.setScope(scope);
  await m.preview();
  expect(m.cleanup.phase).toBe('previewed');
  expect(m.cleanup.preview!.total).toBe(total);
  expect(m.cleanup.preview!.rows.every((r) => r.samples.length > 0)).toBe(true);
});

it('previewed → idle when the scope changes, so run is refused again', async () => {
  await m.preview();
  m.setScope({ folder: 'all' });
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.preview).toBeNull();
  expect(await m.run()).toBe(false);
});

it('previewed → running → done', async () => {
  await m.preview();
  expect(await m.run()).toBe(true);
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.batch).toMatchObject({ status: 'running', done: 0, total: 412 });

  await vi.advanceTimersByTimeAsync(160);
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.batch!.done).toBe(29);
  expect(m.cleanup.batch!.tokens).toBeGreaterThan(0);
  expect(m.cleanup.batch!.costUsd).toBeGreaterThan(0);

  await finish();
  expect(m.cleanup.phase).toBe('done');
  expect(m.cleanup.preview).toBeNull();
  expect(m.cleanup.batches[0]).toMatchObject({ id: m.cleanup.batch!.id, status: 'done', done: 412 });
  expect(toast.text).toBe('Cleanup done: 412 emails sorted. Undo it as one batch below.');

  // Only the toast's timer is left; the progress timer was cleared.
  expect(vi.getTimerCount()).toBe(1);
});

it('running refuses a new scope, a new preview and a second run', async () => {
  await m.preview();
  await m.run();
  m.setScope({ range: 'all' });
  await m.preview();
  expect(await m.run()).toBe(false);
  expect(m.cleanup.phase).toBe('running');
  expect(m.cleanup.scope.range).toBe('30');
  expect(m.cleanup.preview!.total).toBe(412);
  await finish();
});

it('done → run is refused until a new preview', async () => {
  await m.preview();
  await m.run();
  await finish();
  expect(await m.run()).toBe(false);
  await m.preview();
  expect(m.cleanup.phase).toBe('previewed');
  expect(await m.run()).toBe(true);
  await finish();
  expect(m.cleanup.batches).toHaveLength(2);
});

it('undo marks a past batch undone', async () => {
  await m.load();
  expect(m.cleanup.batches).toMatchObject([{ id: 'k1', status: 'done' }]);
  await m.undo('k1');
  expect(m.cleanup.batches[0].status).toBe('undone');
  expect(toast.text).toBe('412 emails moved back where they were');
});

it('done → idle when the batch just run is undone', async () => {
  await m.preview();
  await m.run();
  await finish();
  await m.undo(m.cleanup.batch!.id);
  expect(m.cleanup.phase).toBe('idle');
  expect(m.cleanup.batches[0].status).toBe('undone');
});
