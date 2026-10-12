import { api } from './client';
import type { components } from './schema';

export type Limits = components['schemas']['Limits'];

/** The tenant's use against the hosted service's limits; both are null on self-host. */
export const get = () => api<Limits>('GET', '/limits');
