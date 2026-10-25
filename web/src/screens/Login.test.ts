import { fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { auth, logout, navigate, start } from '../lib/state/auth.svelte';
import Login from './Login.svelte';

let meReply: [status: number, body: unknown];
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const key = `${init.method} ${url}`;
  if (key === 'POST /api/auth/logout') return new Response(null, { status: 204 });
  if (key !== 'GET /api/auth/me') throw new Error(`unexpected ${key}`);
  return new Response(JSON.stringify(meReply[1]), { status: meReply[0] });
});
const unauthenticated = (extra: object = {}) => ({ error: { code: 'unauthenticated', message: 'Sign in first.', ...extra } });

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(auth, { status: 'loading', user: null, members: 1, signIn: null, signOut: null, endedHere: false });
  history.replaceState(null, '', '/');
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it('goes straight to the sign-in page, replacing this one, when the daemon names one', async () => {
  meReply = [401, unauthenticated({ sign_in: '/auth/workos/start' })];
  const replace = vi.spyOn(navigate, 'replace').mockImplementation(() => {});
  await start();
  render(Login);
  expect(replace).toHaveBeenCalledExactlyOnceWith('/auth/workos/start');
  expect(screen.queryByRole('heading')).toBeNull();
  expect(screen.queryByRole('button')).toBeNull();
  expect(screen.queryByLabelText('Password')).toBeNull();
});

it('waits for Continue, with no password form, after this tab signed out', async () => {
  meReply = [200, { user: { id: 1, email: 'me@example.com' }, members: 1 }];
  await start();
  const replace = vi.spyOn(navigate, 'replace').mockImplementation(() => {});
  const go = vi.spyOn(navigate, 'to').mockImplementation(() => {});
  auth.signIn = '/auth/workos/start';
  await logout();
  render(Login);
  expect(replace).not.toHaveBeenCalled();
  expect(screen.getByRole('heading').textContent).toBe('Sign in');
  expect(screen.queryByLabelText('Password')).toBeNull();
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(go).toHaveBeenCalledExactlyOnceWith('/auth/workos/start');
});

it.each([
  ['setup_required', 'Set up MailRules', 'Create account'],
  ['unauthenticated', 'Sign in', 'Sign in'],
])('keeps the password form for %s when no sign-in page is named', async (code, heading, button) => {
  meReply = [401, { error: { code, message: 'x' } }];
  await start();
  render(Login);
  expect(screen.getByRole('heading').textContent).toBe(heading);
  expect(screen.getByLabelText(/Password/)).toBeTruthy();
  expect(screen.getByRole('button', { name: button })).toBeTruthy();
  expect(screen.queryByRole('button', { name: 'Continue' })).toBeNull();
});

it.each([
  ['unavailable', "We couldn't reach the sign-in service. Try again in a minute."],
  ['refused', "That sign-in didn't go through. Try again."],
  ['choose_tenant', 'Your account belongs to more than one organisation. Choose one to continue.'],
  ['tenant_mismatch', 'This account signed up under a different organisation. Sign in with that one.'],
  ['email_in_use', 'Another MailRules account already uses this email address.'],
  ['seats_full', "Every seat of your team's plan is taken. Ask an Owner or Admin of your team to invite you: an invitation adds a seat."],
  ['removed', 'You were removed from this team. Ask an Owner or Admin of the team to invite you again.'],
  ['something_new', "That sign-in didn't go through. Try again."],
])('stays to show the message for ?signin_error=%s and clears it from the address bar', async (code, message) => {
  history.replaceState(null, '', `/?signin_error=${code}&keep=1#/`);
  meReply = [401, unauthenticated({ sign_in: '/auth/workos/start' })];
  const replace = vi.spyOn(navigate, 'replace').mockImplementation(() => {});
  const go = vi.spyOn(navigate, 'to').mockImplementation(() => {});
  await start();
  render(Login);
  expect(replace).not.toHaveBeenCalled();
  expect(screen.getByRole('alert').textContent).toBe(message);
  expect(location.search).toBe('?keep=1');
  expect(location.hash).toBe('#/');
  await fireEvent.click(screen.getByRole('button', { name: 'Continue' }));
  expect(go).toHaveBeenCalledExactlyOnceWith('/auth/workos/start');
});
