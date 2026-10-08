import { api, query } from './client';
import type { components, operations } from './schema';

export type Batch = components['schemas']['Batch'];
export type UndoResult = components['schemas']['UndoResult'];
export type CleanupCheck = components['schemas']['CleanupCheck'];
export type CleanupCheckRow = components['schemas']['CleanupCheckRow'];
export type CleanupCheckRequest = components['schemas']['CleanupCheckRequest'];
export type CleanupRunRequest = components['schemas']['CleanupRunRequest'];
export type CleanupRunItem = components['schemas']['CleanupRunItem'];
export type CleanupSelectionRequest = components['schemas']['CleanupSelectionRequest'];
export type Folder = components['schemas']['Folder'];
type Page = operations['listBatches']['responses'][200]['content']['application/json'];

/**
 * Run one real, paid check of the selection for each mailbox named (`account_id`, or `account_ids` for
 * several together), optionally limited to some rules; the daemon keeps them in memory and goes on in
 * the background. Answers the running checks in the order asked.
 */
export const startChecks = (r: CleanupCheckRequest) => api<{ checks: CleanupCheck[] }>('POST', '/cleanup/check', r).then((x) => x.checks);
/** Every mailbox's current check, with its rows when ready or stale; empty when there is none. */
export const listChecks = () => api<{ checks: CleanupCheck[] }>('GET', '/cleanup/checks').then((x) => x.checks);
/** Throw the account's check away. */
export const discardCheck = (accountId: number) => api<void>('DELETE', '/cleanup/check' + query({ account_id: accountId }));
/** Keep the user's unticked rows with the check, so a reload shows the same ticks; Sort takes its own list. */
export const saveSelection = (r: CleanupSelectionRequest) => api<void>('PUT', '/cleanup/check/selection', r);
/**
 * Sort the kept rows of finished checks, one batch per mailbox, all started or none; replays their
 * saved decisions, no model calls. Answers the batches in the order asked.
 */
export const runSort = (runs: CleanupRunItem[]) => api<{ batches: Batch[] }>('POST', '/cleanup/run', { runs } satisfies CleanupRunRequest).then((x) => x.batches);
/** Past cleanup runs, newest first. */
export const list = (cursor?: string) => api<Page>('GET', '/batches' + query({ kind: 'cleanup', cursor }));
export const get = (id: number) => api<{ batch: Batch }>('GET', `/batches/${id}`).then((x) => x.batch);
export const undo = (id: number) => api<UndoResult>('POST', `/batches/${id}/undo`);
/** The mailbox's folders, for the folder choice in the scope. */
export { folders } from './accounts';
