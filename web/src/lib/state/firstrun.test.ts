import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { login, logout, setup } from './auth.svelte';
import { close, firstRun, next, settle, skip, type Step } from './firstrun.svelte';

type Reply = [status: number, body?: unknown];
let routes: Record<string, Reply>;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const r = routes[`${init.method} ${url}`];
  if (!r) throw new Error(`unexpected ${init.method} ${url}`);
  return new Response(r[1] === undefined ? null : JSON.stringify(r[1]), { status: r[0] });
});
const session = { user: { id: 1, email: 'me@example.com' } };
const creds = { email: 'me@example.com', password: 'a long enough password' };

/** Waits for the router to land on `at`. */
const landsOn = (at: string) => vi.waitFor(() => expect(location.hash).toBe(at));

beforeEach(() => {
  routes = { 'POST /api/auth/setup': [201, session], 'POST /api/auth/login': [200, session], 'POST /api/auth/logout': [204] };
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(firstRun, { offered: false, step: null });
  location.hash = '#/';
});
afterEach(() => vi.unstubAllGlobals());

it.each<[who: string, signIn: typeof setup, mailboxes: number, step: Step | null, at: string]>([
  ['an admin account just created here, on an install with no mailbox', setup, 0, 'ai', '#/setup'],
  ['an admin account just created here, on an install that already has a mailbox', setup, 1, null, '#/'],
  ['a sign-in to an existing install, even with no mailbox', login, 0, null, '#/'],
])('opens the guide for %s: %s', async (_who, signIn, mailboxes, step, at) => {
  await signIn(creds);
  settle(mailboxes);
  expect(firstRun.step).toBe(step);
  await landsOn(at);
});

it('settles an offer once: listing the mailboxes again does not reopen a closed guide', async () => {
  await setup(creds);
  settle(0);
  close('/');
  settle(0);
  expect(firstRun.step).toBeNull();
});

it('forgets the guide on sign-out, so signing in again does not show it', async () => {
  await setup(creds);
  settle(0);
  await logout();
  await login(creds);
  settle(0);
  expect(firstRun).toEqual({ offered: false, step: null });
});

it.each<[from: Step, action: string, act: () => void, step: Step | null, at: string]>([
  ['ai', 'Continue', next, 'mailbox', '#/setup'],
  ['ai', 'Skip for now', skip, 'mailbox', '#/setup'],
  ['mailbox', 'a connected mailbox', next, 'done', '#/setup'],
  ['mailbox', 'Skip for now', skip, null, '#/'],
  ['ai', 'Skip setup', () => close('/'), null, '#/'],
  ['done', 'Open Activity', () => close('/activity'), null, '#/activity'],
])('from the %s step, %s leads on', async (from, _action, act, step, at) => {
  location.hash = '#/setup';
  firstRun.step = from;
  act();
  expect(firstRun.step).toBe(step);
  await landsOn(at);
});
