import * as authApi from '../api/auth';
import { ApiError, setUnauthorizedHandler } from '../api/client';

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
});

/** Ask the daemon who is signed in; a 401 routes to Setup or Login through the handler above. */
export async function start() {
  try {
    signedIn(await authApi.me());
  } catch (e) {
    if (!(e instanceof ApiError && e.status === 401)) throw e;
  }
}

export const setup = async (c: authApi.Credentials) => signedIn(await authApi.setup(c));
export const login = async (c: authApi.Credentials) => signedIn(await authApi.login(c));

export async function logout() {
  await authApi.logout();
  auth.user = null;
  auth.status = 'login';
}
