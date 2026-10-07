import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import Nav from './Nav.svelte';

const menu = () => screen.getByRole('button', { name: 'Menu' });

it.each([
  { how: 'pressing Menu again', close: () => fireEvent.click(menu()) },
  { how: 'choosing a screen', close: () => fireEvent.click(screen.getByRole('link', { name: 'Rules' })) },
  { how: 'pressing Escape', close: () => fireEvent.keyDown(window, { key: 'Escape' }) },
])('the phone menu opens from Menu and closes on $how', async ({ close }) => {
  render(Nav);
  expect(menu().getAttribute('aria-expanded')).toBe('false');
  await fireEvent.click(menu());
  expect(menu().getAttribute('aria-expanded')).toBe('true');
  await close();
  expect(menu().getAttribute('aria-expanded')).toBe('false');
});
