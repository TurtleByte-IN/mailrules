import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { error, item, serve, stats } from '../lib/state/activity.fixtures';
import { accounts } from '../lib/state/accounts.svelte';
import { overview } from '../lib/state/overview.svelte';
import Overview from './Overview.svelte';

beforeEach(() => {
  Object.assign(overview, { stats: null, latest: [], error: '' });
  Object.assign(accounts, { list: [], loaded: true, error: '' });
});
afterEach(() => vi.unstubAllGlobals());

const daemon = (up: () => boolean) =>
  serve((call) =>
    !up()
      ? [500, error('internal', 'Something went wrong.')]
      : call.includes('/stats/summary')
        ? [200, stats]
        : [200, { items: [item()], next_cursor: null }],
  );

it('shows an alert with Retry when today\'s numbers fail to load, and Retry loads them', async () => {
  let up = false;
  daemon(() => up);
  render(Overview);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');
  expect(screen.queryByText('Processed today')).toBeNull();

  up = true;
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Processed today')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('shows today\'s numbers, the top rule and the latest decision', async () => {
  daemon(() => true);
  render(Overview);
  expect(await screen.findByText('40 processed')).toBeTruthy();
  expect(screen.getByText('jev 31 calls')).toBeTruthy();
  expect(screen.getByText('Recruiters')).toBeTruthy();
  expect(screen.getByText('Senior backend role')).toBeTruthy();
  expect(screen.getByText('No mailbox connected yet.')).toBeTruthy();
  // The bar's four parts are the daemon's own split, not arithmetic on the counts.
  expect(document.body.textContent).toContain('Left in Inbox 6');
  expect(document.body.textContent).toContain('Sorted 29');
});

it("sends Reconnect of a mailbox a module's sign-in form connected to that form on Mailboxes, with its id", async () => {
  daemon(() => true);
  const proton = { name: 'proton', label: 'Proton Mail', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', help_url: '', local_part_login: false, secret_label: 'Bridge password',
    password: false, one_click_url: null, form_url: '/api/proton/sign-in' } as const;
  accounts.presets = [proton];
  accounts.list = [{ id: 5, label: 'me@proton.me', preset: 'proton', host: '127.0.0.1', port: 1143, tls_mode: 'starttls', username: 'me@proton.me', watch_folder: 'INBOX',
    status: 'reconnect_needed', last_error: '', last_event_at: null, last_mail_at: null, capabilities: [], can_move: true, folder_count: 2, created_at: 1, shared: false, mine: true,
    cert_fingerprint: '', one_click: false, module: true }];
  render(Overview);
  expect((await screen.findByRole('link', { name: 'Reconnect me@proton.me' })).getAttribute('href')).toBe('#/accounts?reconnect=5');
  accounts.presets = [];
});

it.each([
  ['gmail', 'Gmail', '#/accounts?reconnect=6'],
  ['outlook', 'Outlook', '/api/oauth/start?provider=outlook&account=6'],
] as const)("sends Reconnect of a one-click %s mailbox to Mailboxes for the note before Google's sign-in, or straight to the provider", async (name, label, href) => {
  daemon(() => true);
  accounts.presets = [{ name, label, host: 'imap.example.com', port: 993, tls_mode: 'implicit', help_url: '', local_part_login: false, secret_label: 'App password',
    password: true, one_click_url: '/api/oauth/start?provider=' + name, form_url: null }];
  accounts.list = [{ id: 6, label: 'me@example.com', preset: name, host: 'imap.example.com', port: 993, tls_mode: 'implicit', username: 'me@example.com', watch_folder: 'INBOX',
    status: 'reconnect_needed', last_error: '', last_event_at: null, last_mail_at: null, capabilities: [], can_move: true, folder_count: 2, created_at: 1, shared: false, mine: true,
    cert_fingerprint: '', one_click: true, module: true }];
  render(Overview);
  expect((await screen.findByRole('link', { name: 'Reconnect me@example.com' })).getAttribute('href')).toBe(href);
  accounts.presets = [];
});
