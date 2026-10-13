import { fireEvent, render, screen } from '@testing-library/svelte';
import { expect, it } from 'vitest';
import { auth } from '../state/auth.svelte';
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

// Members shows while the plan includes the team; Plan and billing only to an Owner, and the
// dry-run switch is the team's, so a Member sees it but cannot flip it.
it.each([
  ['owner', true, false],
  ['admin', false, false],
  ['member', false, true],
] as const)('shows an %s Members, Plan and billing: %s, and a locked dry-run switch: %s', async (role, plan, locked) => {
  auth.user = { id: 1, email: 'ana@example.com', role };
  Object.assign(settings.value.features, { billing: true, team: true });
  try {
    render(Nav);
    expect(screen.getByRole('link', { name: 'Members' })).toBeTruthy();
    expect(!!screen.queryByRole('link', { name: 'Plan and billing' })).toBe(plan);
    expect((screen.getByRole('button', { name: 'Dry-run' }) as HTMLButtonElement).disabled).toBe(locked);
  } finally {
    Object.assign(settings.value.features, { billing: false, team: false });
    auth.user = null;
  }
});
