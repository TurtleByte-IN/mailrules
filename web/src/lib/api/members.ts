// The team: members, invitations and the audit log. Only while `features.team` is on; elsewhere the routes answer 404 or 403.
import { api } from './client';
import type { components, paths } from './schema';

export type Role = components['schemas']['Role'];
export type Member = components['schemas']['Member'];
export type Invitation = components['schemas']['Invitation'];
export type Team = paths['/api/members']['get']['responses']['200']['content']['application/json'];

export const list = () => api<Team>('GET', '/members');
/** Admin and up; only an Owner sets or changes an Owner's role. */
export const setRole = (id: number, role: Role) => api<{ member: Member }>('PATCH', `/members/${id}`, { role });
export const remove = (id: number) => api<void>('DELETE', `/members/${id}`);

export const invitations = () => api<{ invitations: Invitation[] }>('GET', '/invitations');
export const invite = (email: string, role: Role) => api<{ invitation: Invitation }>('POST', '/invitations', { email, role });
export const revoke = (id: number) => api<void>('DELETE', `/invitations/${id}`);
