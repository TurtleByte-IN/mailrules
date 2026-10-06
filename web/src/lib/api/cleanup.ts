import { api, query } from './client';
import type { components, operations } from './schema';

export type Batch = components['schemas']['Batch'];
export type UndoResult = components['schemas']['UndoResult'];
export type CleanupRequest = components['schemas']['CleanupRequest'];
export type Preview = components['schemas']['CleanupPreview'];
export type Folder = components['schemas']['Folder'];
type Page = operations['listBatches']['responses'][200]['content']['application/json'];

export const preview = (r: CleanupRequest) => api<Preview>('POST', '/cleanup/preview', r);
export const run = (r: CleanupRequest) => api<{ batch: Batch }>('POST', '/cleanup/run', r).then((x) => x.batch);
/** Past cleanup runs, newest first. */
export const list = (cursor?: string) => api<Page>('GET', '/batches' + query({ kind: 'cleanup', cursor }));
export const get = (id: number) => api<{ batch: Batch }>('GET', `/batches/${id}`).then((x) => x.batch);
export const undo = (id: number) => api<UndoResult>('POST', `/batches/${id}/undo`);
/** The mailbox's folders, for the folder choice in the scope. */
export const folders = (accountId: number) => api<{ items: Folder[] }>('GET', `/accounts/${accountId}/folders`).then((x) => x.items);
