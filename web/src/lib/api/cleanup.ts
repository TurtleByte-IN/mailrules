import { api, query } from './client';
import type { components, operations } from './schema';

export type Batch = components['schemas']['Batch'];
export type UndoResult = components['schemas']['UndoResult'];
export type CleanupCheck = components['schemas']['CleanupCheck'];
export type CleanupCheckRow = components['schemas']['CleanupCheckRow'];
export type CleanupCheckRequest = components['schemas']['CleanupCheckRequest'];
export type CleanupRunRequest = components['schemas']['CleanupRunRequest'];
export type CleanupSelectionRequest = components['schemas']['CleanupSelectionRequest'];
export type Folder = components['schemas']['Folder'];
type Page = operations['listBatches']['responses'][200]['content']['application/json'];

/** Run one real, paid check of the selection; the daemon keeps it in memory and goes on in the background. */
export const startCheck = (r: CleanupCheckRequest) => api<{ check: CleanupCheck }>('POST', '/cleanup/check', r).then((x) => x.check);
/** The account's current check, with its rows when ready or stale; null when there is none. */
export const getCheck = (accountId: number) =>
  api<{ check: CleanupCheck | null }>('GET', '/cleanup/check' + query({ account_id: accountId })).then((x) => x.check);
/** Throw the account's check away. */
export const discardCheck = (accountId: number) => api<void>('DELETE', '/cleanup/check' + query({ account_id: accountId }));
/** Keep the user's unticked rows with the check, so a reload shows the same ticks; Sort takes its own list. */
export const saveSelection = (r: CleanupSelectionRequest) => api<void>('PUT', '/cleanup/check/selection', r);
/** Sort the kept rows of a check; replays its saved decisions, no model calls. */
export const runSort = (r: CleanupRunRequest) => api<{ batch: Batch }>('POST', '/cleanup/run', r).then((x) => x.batch);
/** Past cleanup runs, newest first. */
export const list = (cursor?: string) => api<Page>('GET', '/batches' + query({ kind: 'cleanup', cursor }));
export const get = (id: number) => api<{ batch: Batch }>('GET', `/batches/${id}`).then((x) => x.batch);
export const undo = (id: number) => api<UndoResult>('POST', `/batches/${id}/undo`);
/** The mailbox's folders, for the folder choice in the scope. */
export { folders } from './accounts';
