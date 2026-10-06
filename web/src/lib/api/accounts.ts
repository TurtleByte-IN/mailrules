// DEMO: none of these are in api/openapi.yaml yet (backend M7):
//   GET /api/presets, POST /api/accounts/test, GET POST /api/accounts,
//   GET PATCH DELETE /api/accounts/{id}, POST /api/accounts/{id}/reconnect,
//   GET /api/accounts/{id}/folders.
// GET and PATCH on one account and the folder list have no caller in the UI yet, so
// they have no function here; the folder count rides on Account.
import { fake, fakeId } from './demo';

export interface Account {
  id: string;
  email: string;
  provider: string;
  status: 'live' | 'reconnecting' | 'error';
  /** Folders found by discovery. */
  folders: number;
  /** Unix seconds of the last thing the watcher saw; null before the first one. */
  lastEventAt: number | null;
  /** Set while status is 'error'. */
  lastError?: string;
}

export interface Preset {
  /** icloud | fastmail | yahoo | zoho | generic, as in the accounts table. */
  id: string;
  name: string;
  note: string;
  /** What the provider calls the secret the user pastes. */
  secretLabel: string;
  /** False when the user has to type host and port. */
  knowsHost: boolean;
  /** False for sign-in methods that are not built yet. */
  available: boolean;
}

/** The password is sent once and never comes back in any response. */
export interface Credentials {
  preset: string;
  email: string;
  password: string;
  host?: string;
  port?: number;
}

export interface TestResult {
  folders: number;
  /** Special-use folders detected, e.g. Junk, Trash, Archive. */
  special: string[];
  idle: boolean;
}

const presets: Preset[] = [
  { id: 'icloud', name: 'iCloud', note: 'App-specific password', secretLabel: 'App-specific password', knowsHost: true, available: true },
  { id: 'fastmail', name: 'Fastmail', note: 'App password', secretLabel: 'App password', knowsHost: true, available: true },
  { id: 'yahoo', name: 'Yahoo', note: 'App password', secretLabel: 'App password', knowsHost: true, available: true },
  { id: 'zoho', name: 'Zoho', note: 'App password', secretLabel: 'App password', knowsHost: true, available: true },
  { id: 'generic', name: 'Other IMAP', note: 'Host, port, password', secretLabel: 'Password', knowsHost: false, available: true },
  { id: 'oauth', name: 'Gmail and Outlook', note: 'One-click sign-in, coming soon', secretLabel: '', knowsHost: true, available: false },
];

const now = () => Math.floor(Date.now() / 1000);

let accounts: Account[] = [
  { id: 'acc1', email: 'me@icloud.com', provider: 'iCloud', status: 'live', folders: 14, lastEventAt: now() - 240 },
  { id: 'acc2', email: 'work@fastmail.com', provider: 'Fastmail', status: 'live', folders: 22, lastEventAt: now() - 1140 },
];

const found: TestResult = { folders: 14, special: ['Junk', 'Trash', 'Archive'], idle: true };

export const listPresets = () => fake(presets);
export const list = () => fake(accounts);

/** Tries the credentials without saving anything. The demo accepts any and keeps none. */
export const test = (_c: Credentials) => fake(found);

/** Re-checks a connected mailbox with the credentials the daemon already holds. */
export const testSaved = (_id: string) => fake(found);

export function create(c: Credentials) {
  const account: Account = {
    id: fakeId('acc'),
    email: c.email,
    provider: presets.find((p) => p.id === c.preset)?.name ?? c.preset,
    status: 'live',
    folders: found.folders,
    lastEventAt: null,
  };
  accounts = [...accounts, account];
  return fake(account);
}

export function reconnect(id: string) {
  accounts = accounts.map((a) => (a.id === id ? { ...a, status: 'live', lastError: undefined, lastEventAt: now() } : a));
  return fake(accounts.find((a) => a.id === id)!);
}

export function remove(id: string) {
  accounts = accounts.filter((a) => a.id !== id);
  return fake(undefined);
}
