// GET one account has no caller in the UI yet, so it has no function here.
import { api } from './client';
import type { components } from './schema';

type S = components['schemas'];
export type Account = S['Account'];
export type AccountStatus = S['AccountStatus'];
export type Preset = S['Preset'];
export type AccountPatch = S['AccountPatch'];
/** The password is sent once and never comes back in any response. */
export type AccountInput = S['AccountInput'];
export type TestResult = S['AccountTestResult'];

export const listPresets = () => api<{ items: Preset[] }>('GET', '/presets').then((r) => r.items);
export const list = () => api<{ items: Account[] }>('GET', '/accounts').then((r) => r.items);
/** The folders as last discovered on the server. */
export const folders = (id: number) => api<{ items: S['Folder'][] }>('GET', `/accounts/${id}/folders`).then((r) => r.items);

/** Tries the credentials without saving anything. */
export const test = (c: AccountInput) => api<TestResult>('POST', '/accounts/test', c);
/** Tries the stored login on a connection of its own; the watcher and the status are left alone. */
export const testStored = (id: number) => api<TestResult>('POST', `/accounts/${id}/test`);

const one = (r: S['AccountEnvelope']) => r.account;
export const create = (c: AccountInput) => api<S['AccountEnvelope']>('POST', '/accounts', c).then(one);
export const patch = (id: number, p: AccountPatch) => api<S['AccountEnvelope']>('PATCH', `/accounts/${id}`, p).then(one);
/** Answers at once; the outcome arrives as an account.status event. */
export const reconnect = (id: number) => api<S['AccountEnvelope']>('POST', `/accounts/${id}/reconnect`).then(one);
export const remove = (id: number) => api<void>('DELETE', `/accounts/${id}`);
