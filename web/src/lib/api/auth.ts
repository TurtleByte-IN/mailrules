import { api } from './client';
import type { components } from './schema';

export type Session = components['schemas']['Session'];
export type Credentials = components['schemas']['Credentials'];

export const me = () => api<Session>('GET', '/auth/me');
export const setup = (c: Credentials) => api<Session>('POST', '/auth/setup', c);
export const login = (c: Credentials) => api<Session>('POST', '/auth/login', c);
export const logout = () => api<void>('POST', '/auth/logout');
