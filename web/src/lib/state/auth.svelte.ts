import * as authApi from '../api/auth';
import { ApiError, setUnauthorizedHandler } from '../api/client';
import { forget, offer } from './firstrun.svelte';

type Status = 'loading' | 'setup' | 'login' | 'in';

export const auth = $state<{
  status: Status;
  user: authApi.Session['user'] | null;
  /** Users in the signed-in user's team; sharing a mailbox is offered when it is more than one. */
  members: number;
  /** Set when a sign-in module is present: the page that signs someone in. No password form then. */
  signIn: string | null;
  /** Set when a sign-in module is present: the page to go to after signing out. */
  signOut: string | null;
}>({
  status: 'loading',
  user: null,
  members: 1,
  signIn: null,
  signOut: null,
});

function signedIn(s: authApi.Session) {
  auth.user = s.user;
  auth.members = s.members;
  auth.signOut = s.sign_out || null;
  auth.status = 'in';
}

setUnauthorizedHandler((code, signIn) => {
  auth.user = null;
  // Only GET /api/auth/me carries the sign-in path; a later 401 elsewhere keeps the one already known.
  if (signIn) auth.signIn = signIn;
  auth.status = code === 'setup_required' && !auth.signIn ? 'setup' : 'login';
  forget();
});

/** Ask the daemon who is signed in; a 401 routes to Setup or Login through the handler above. */
export async function start() {
  try {
    signedIn(await authApi.me());
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) throw e;
  }
}

/** Creates the admin account and signs in; the first-run guide is offered to this browser only. */
export async function setup(c: authApi.Credentials) {
  signedIn(await authApi.setup(c));
  offer();
}
export const login = async (c: authApi.Credentials) => signedIn(await authApi.login(c));

/** Full-page navigation, behind an object so tests can watch it. */
export const navigate = { to: (url: string) => window.location.assign(url) };

export async function logout() {
  await authApi.logout();
  const to = auth.signOut;
  auth.user = null;
  auth.status = 'login';
  forget();
  if (to) navigate.to(to);
}
