import { api, query } from './client';
import type { components, operations } from './schema';

export type Sender = components['schemas']['Sender'];
export type SenderPut = components['schemas']['SenderPut'];
export type ListParams = NonNullable<operations['listSenders']['parameters']['query']>;
export type Sort = NonNullable<ListParams['sort']>;
type Page = operations['listSenders']['responses'][200]['content']['application/json'];
type Key = Pick<Sender, 'type' | 'value'>;

const path = (s: Key) => `/senders/${s.type}/${encodeURIComponent(s.value)}`;

export const list = (params: ListParams = {}) => api<Page>('GET', '/senders' + query(params));
export const put = (s: Key, body: SenderPut) => api<{ sender: Sender }>('PUT', path(s), body).then((r) => r.sender);
export const remove = (s: Key) => api<void>('DELETE', path(s));
