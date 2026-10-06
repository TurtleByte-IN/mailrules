import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it, vi } from 'vitest';
import Toggle from './Toggle.svelte';

it('says whether it is on and calls back when pressed', async () => {
  const onchange = vi.fn();
  render(Toggle, { on: true, label: 'Dry-run', onchange });
  const button = screen.getByRole('button', { name: 'Dry-run' });
  expect(button.getAttribute('aria-pressed')).toBe('true');
  await fireEvent.click(button);
  expect(onchange).toHaveBeenCalledOnce();
});
