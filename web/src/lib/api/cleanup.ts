import { api } from './client';
import type { components } from './schema';

export type Batch = components['schemas']['Batch'];
export type UndoResult = components['schemas']['UndoResult'];
export type CleanupRequest = components['schemas']['CleanupRequest'];
export type Preview = components['schemas']['CleanupPreview'];
export type Folder = components['schemas']['Folder'];

export const preview = (r: CleanupRequest) => api<Preview>('POST', '/cleanup/preview', r);
export const run = (r: CleanupRequest) => api<{ batch: Batch }>('POST', '/cleanup/run', r).then((x) => x.batch);
export const get = (id: number) => api<{ batch: Batch }>('GET', `/batches/${id}`).then((x) => x.batch);
export const undo = (id: number) => api<UndoResult>('POST', `/batches/${id}/undo`);
/** The mailbox's folders, for the folder choice in the scope. */
export const folders = (accountId: number) => api<{ items: Folder[] }>('GET', `/accounts/${accountId}/folders`).then((x) => x.items);
