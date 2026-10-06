import { api, query } from './client';
import type { components, operations } from './schema';

type Schemas = components['schemas'];
/** The 200 body of an operation. */
export type Ok<K extends keyof operations> = operations[K]['responses'] extends { 200: { content: { 'application/json': infer T } } } ? T : never;

export type ActivityItem = Schemas['ActivityItem'];
export type MessageDetail = Schemas['MessageDetail'];
export type MessageAction = Schemas['MessageAction'];
export type TraceStep = Schemas['TraceStep'];
export type FixRequest = Schemas['FixRequest'];
export type StatsSummary = Schemas['StatsSummary'];
/** Filters, cursor and limit of the feed. */
export type ActivityQuery = NonNullable<operations['listActivity']['parameters']['query']>;

export const list = (q: ActivityQuery = {}) => api<Ok<'listActivity'>>('GET', '/activity' + query(q));
export const get = async (id: number) => (await api<Ok<'getMessage'>>('GET', `/messages/${id}`)).message;
export const correct = (id: number, fix: FixRequest) => api<Ok<'correctMessage'>>('POST', `/messages/${id}/correct`, fix);
/** Undo everything still in effect on one email, in one call. */
export const undoMessage = (id: number) => api<Ok<'undoMessage'>>('POST', `/messages/${id}/undo`);
/** Undo everything done at or after a unix time. */
export const undoSince = (since: number) => api<Ok<'undoSince'>>('POST', '/actions/undo' + query({ since }));
/** Today's numbers for the Activity tiles. */
export const summary = () => api<Ok<'statsSummary'>>('GET', '/stats/summary' + query({ range: 'day' }));
