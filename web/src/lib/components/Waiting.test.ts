import { render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import Waiting from './Waiting.svelte';

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

it('says what is going on, and counts the seconds once it takes a few', async () => {
  const { container } = render(Waiting, { text: 'Logging in to your mailbox' });
  expect(screen.getByRole('status').textContent).toBe('Logging in to your mailbox…');
  await vi.advanceTimersByTimeAsync(3000);
  expect(container.textContent).not.toMatch(/\ds/);
  await vi.advanceTimersByTimeAsync(2000);
  expect(container.textContent).toContain('5s');
  await vi.advanceTimersByTimeAsync(7000);
  expect(container.textContent).toContain('12s');
  // The count is not in the live region: it would be read out every second.
  expect(screen.getByRole('status').textContent).toBe('Logging in to your mailbox…');
});

it('stops counting when it goes away', async () => {
  const clear = vi.spyOn(globalThis, 'clearInterval');
  const { unmount } = render(Waiting, { text: 'x' });
  unmount();
  expect(clear).toHaveBeenCalled();
});
