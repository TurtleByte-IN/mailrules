// GET one account, the label, watch-folder and password edits of PATCH, and the folder
// list have no caller in the UI yet, so they have no function here.
import { api } from './client';
import type { components } from './schema';

type S = components['schemas'];
export type Account = S['Account'];
export type AccountStatus = S['AccountStatus'];
export type Preset = S['Preset'];
/**
 * The password is sent once and never comes back in any response. watch_folder has a default
 * in the contract, which the type generator turns into a required field; it is optional.
 */
export type AccountInput = Omit<S['AccountInput'], 'watch_folder'> & Partial<Pick<S['AccountInput'], 'watch_folder'>>;
export type TestResult = S['AccountTestResult'];

export const listPresets = () => api<{ items: Preset[] }>('GET', '/presets').then((r) => r.items);
export const list = () => api<{ items: Account[] }>('GET', '/accounts').then((r) => r.items);

/** Tries the credentials without saving anything. */
export const test = (c: AccountInput) => api<TestResult>('POST', '/accounts/test', c);

const one = (r: S['AccountEnvelope']) => r.account;
export const create = (c: AccountInput) => api<S['AccountEnvelope']>('POST', '/accounts', c).then(one);
export const patch = (id: number, p: S['AccountPatch']) => api<S['AccountEnvelope']>('PATCH', `/accounts/${id}`, p).then(one);
/** Answers at once; the outcome arrives as an account.status event. */
export const reconnect = (id: number) => api<S['AccountEnvelope']>('POST', `/accounts/${id}/reconnect`).then(one);
export const remove = (id: number) => api<void>('DELETE', `/accounts/${id}`);
