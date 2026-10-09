import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { auth, logout, navigate, start } from './auth.svelte';

let me: unknown;
const fetchMock = vi.fn(async (url: string, init: RequestInit) => {
  const key = `${init.method} ${url}`;
  if (key === 'GET /api/auth/me') return new Response(JSON.stringify(me), { status: 200 });
  if (key === 'POST /api/auth/logout') return new Response(null, { status: 204 });
  throw new Error(`unexpected ${key}`);
});

beforeEach(() => {
  vi.stubGlobal('fetch', fetchMock);
  Object.assign(auth, { status: 'loading', user: null, members: 1, signIn: null, signOut: null });
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

it.each([
  ['goes to the sign-out page the daemon named', '/auth/workos/sign-out', ['/auth/workos/sign-out']],
  ['stays on the sign-in screen when none was named', undefined, []],
])('after signing out it %s', async (_how, signOut, visits) => {
  me = { user: { id: 1, email: 'me@example.com' }, members: 2, sign_out: signOut };
  const go = vi.spyOn(navigate, 'to').mockImplementation(() => {});
  await start();
  expect([auth.status, auth.members]).toEqual(['in', 2]);
  await logout();
  expect(auth.status).toBe('login');
  expect(go.mock.calls.map((c) => c[0])).toEqual(visits);
});
