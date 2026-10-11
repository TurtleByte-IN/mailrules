import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { settings } from '../state/settings.svelte';
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

// The flags come from the daemon's settings (GET /api/settings `features`), not from the web
// build: a screen behind one appears once the loaded settings turn it on.
it('offers a flagged screen only while the settings say its feature is on', async () => {
  render(Nav);
  expect(screen.queryByRole('link', { name: 'Plan and billing' })).toBeNull();
  settings.value.features.billing = true;
  try {
    expect(await screen.findByRole('link', { name: 'Plan and billing' })).toBeTruthy();
  } finally {
    settings.value.features.billing = false;
  }
});
