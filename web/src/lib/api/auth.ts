import { api } from './client';
import type { components } from './schema';

export type Session = components['schemas']['Session'];
export type Credentials = components['schemas']['Credentials'];

export const me = () => api<Session>('GET', '/auth/me');
export const setup = (c: Credentials) => api<Session>('POST', '/auth/setup', c);
export const login = (c: Credentials) => api<Session>('POST', '/auth/login', c);
export const logout = () => api<void>('POST', '/auth/logout');
/** Deletes the signed-in person's account; `email` is theirs, typed to confirm. Ends the session. */
export const deleteAccount = (email: string) => api<void>('POST', '/auth/delete-account', { email });
