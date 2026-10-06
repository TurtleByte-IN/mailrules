import { api } from './client';
import type { components, operations } from './schema';

/** A gallery entry; `rule` is ready to send to the batch route. */
export type Template = components['schemas']['Template'];

export const list = async () =>
  (await api<operations['listTemplates']['responses'][200]['content']['application/json']>('GET', '/templates')).items;
