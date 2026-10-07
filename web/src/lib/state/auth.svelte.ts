import * as authApi from '../api/auth';
import { ApiError, setUnauthorizedHandler } from '../api/client';
import { forget, offer } from './firstrun.svelte';

type Status = 'loading' | 'setup' | 'login' | 'in';

export const auth = $state<{ status: Status; user: authApi.Session['user'] | null }>({
  status: 'loading',
  user: null,
});

function signedIn(s: authApi.Session) {
  auth.user = s.user;
  auth.status = 'in';
}

setUnauthorizedHandler((code) => {
  auth.user = null;
  auth.status = code === 'setup_required' ? 'setup' : 'login';
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

export async function logout() {
  await authApi.logout();
  auth.user = null;
  auth.status = 'login';
  forget();
}
