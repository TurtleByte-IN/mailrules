import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ApiError } from '../api/client';
import type { TestProgress } from '../api/rules';
import { settings } from '../state/settings.svelte';
import { testLimit } from '../state/testlimit.svelte';
import TestRunner from './TestRunner.svelte';

type Run = (limit: number, onProgress: (p: TestProgress) => void) => Promise<void>;

beforeEach(() => {
  // The limits GET /api/settings reports.
  Object.assign(settings.value, { limits: { test_default: 200, test_max: 2000, check_max: 2000 } });
  testLimit.value = 200;
});
afterEach(cleanup);

const box = () => screen.getByRole<HTMLInputElement>('spinbutton', { name: 'How many emails to test' });
const enter = (n: string) => fireEvent.input(box(), { target: { value: n } });

/** A run the test finishes by hand. */
function pending() {
  let progress!: (p: TestProgress) => void;
  let finish!: () => void;
  let fail!: (e: unknown) => void;
  const run = vi.fn<Run>(
    (_limit, onProgress) =>
      new Promise<void>((resolve, reject) => {
        progress = onProgress;
        finish = resolve;
        fail = reject;
      }),
  );
  return { run, progress: (p: TestProgress) => progress(p), finish: () => finish(), fail: (e: unknown) => fail(e) };
}

it('starts at 200 and names the number on the button', () => {
  render(TestRunner, { run: vi.fn() });
  expect(box().value).toBe('200');
  expect(screen.getByRole('button', { name: 'Test on last 200 emails' })).toBeTruthy();
});

it('takes 1 up to what the daemon allows', () => {
  render(TestRunner, { run: vi.fn() });
  expect(box().min).toBe('1');
  expect(box().max).toBe('2000');
});

it('follows the limits the daemon reports, not numbers of its own (MAI-41)', async () => {
  Object.assign(settings.value, { limits: { test_default: 300, test_max: 5000, check_max: 5000 } });
  render(TestRunner, { run: vi.fn() });
  expect(box().max).toBe('5000');
  await enter('4000');
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.getByRole('button', { name: 'Test on last 4000 emails' })).toBeTruthy();
  await enter('5001');
  expect(screen.getByRole('alert').textContent).toBe('The limit must be between 1 and 5000.');
});

it('tests the number chosen, and the button says so', async () => {
  const run = vi.fn<Run>(async () => {});
  render(TestRunner, { run });
  await enter('20');
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 20 emails' }));
  expect(run).toHaveBeenCalledOnce();
  expect(run.mock.calls[0][0]).toBe(20);
  await enter('1');
  expect(screen.getByRole('button', { name: 'Test on last 1 email' })).toBeTruthy();
});

it('keeps the number while the app is open: a second screen starts from it', async () => {
  const first = render(TestRunner, { run: vi.fn() });
  await enter('75');
  first.unmount();
  render(TestRunner, { run: vi.fn() });
  expect(box().value).toBe('75');
  expect(screen.getByRole('button', { name: 'Test on last 75 emails' })).toBeTruthy();
});

it.each([['0'], ['2001'], ['-3'], ['1.5'], ['']])('refuses %j beside the box, in the daemon\'s words, and sends nothing', async (value) => {
  const run = vi.fn<Run>(async () => {});
  render(TestRunner, { run });
  await enter(value);
  expect(screen.getByRole('alert').textContent).toBe('The limit must be between 1 and 2000.');
  expect(box().getAttribute('aria-invalid')).toBe('true');
  expect(box().getAttribute('aria-describedby')).toBe(screen.getByRole('alert').id);
  await fireEvent.click(screen.getByRole('button', { name: 'Test' }));
  expect(run).not.toHaveBeenCalled();
  // Back in range: the message and the number on the button return.
  await enter('50');
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.getByRole('button', { name: 'Test on last 50 emails' })).toBeTruthy();
});

it('shows the mailbox being read, then N of M as the daemon reports them', async () => {
  const t = pending();
  render(TestRunner, { run: t.run });
  await enter('20');
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 20 emails' }));

  const button = screen.getByRole<HTMLButtonElement>('button', { name: 'Testing on last 20 emails…' });
  expect(button.disabled).toBe(true);
  expect(box().disabled).toBe(true);
  expect(screen.getByText('Reading your mailbox…')).toBeTruthy();

  t.progress({ done: 0, total: 20, model_calls: 0 });
  expect(await screen.findByText('0 of 20 emails tested')).toBeTruthy();
  expect(screen.queryByText('Reading your mailbox…')).toBeNull();

  t.progress({ done: 7, total: 20, model_calls: 3 });
  expect(await screen.findByText('7 of 20 emails tested · 3 sent to the decision model')).toBeTruthy();
  expect(screen.getByRole('progressbar', { name: 'Test progress' }).getAttribute('aria-valuenow')).toBe('35');

  t.progress({ done: 1500, total: 2000, model_calls: 0 });
  expect(await screen.findByText('1,500 of 2,000 emails tested')).toBeTruthy();

  t.finish();
  expect(await screen.findByRole('button', { name: 'Test on last 20 emails' })).toBeTruthy();
  expect(screen.queryByRole('progressbar')).toBeNull();
  expect(box().disabled).toBe(false);
});

it("puts the daemon's refusal of the number beside the box until the number changes", async () => {
  const t = pending();
  render(TestRunner, { run: t.run });
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  t.fail(new ApiError(400, 'invalid_input', 'The limit must be between 1 and 1000.', 'limit'));

  expect((await screen.findByRole('alert')).textContent).toBe('The limit must be between 1 and 1000.');
  expect(box().getAttribute('aria-invalid')).toBe('true');
  expect(screen.queryByText(/of \d+ emails tested/)).toBeNull();
  expect(screen.getByRole<HTMLButtonElement>('button', { name: 'Test' }).disabled).toBe(false);

  await enter('100');
  expect(screen.queryByRole('alert')).toBeNull();
  expect(screen.getByRole('button', { name: 'Test on last 100 emails' })).toBeTruthy();
});
