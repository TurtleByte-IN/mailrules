import { api } from './client';
import type { components } from './schema';

export type StatsUsage = components['schemas']['StatsUsage'];
export type ModelUsage = components['schemas']['ModelUsage'];

// The contract's only range is month, which is also its default.
export const get = () => api<StatsUsage>('GET', '/stats/usage');
