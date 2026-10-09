import * as accountsApi from '../api/accounts';
import { ApiError } from '../api/client';
import { subscribe } from '../api/events';
import { flash } from './toast.svelte';

export const accounts = $state<{
  list: accountsApi.Account[];
  /** False until the first list arrives, so "not loaded" never reads as "no mailboxes". */
  loaded: boolean;
  error: string;
  presets: accountsApi.Preset[];
  presetsError: string;
}>({ list: [], loaded: false, error: '', presets: [], presetsError: '' });

/** How each status reads and which dot it gets. A record, so a new status fails the build. */
export const statuses: Record<accountsApi.AccountStatus, { label: string; dot: string }> = {
  new: { label: 'Connecting', dot: 'bg-warn-strong' },
  live: { label: 'Live', dot: 'bg-live' },
  reconnecting: { label: 'Reconnecting', dot: 'bg-warn-strong' },
  paused: { label: 'Paused', dot: 'bg-warn-strong' },
  auth_failed: { label: 'Sign-in failed', dot: 'bg-trash' },
  cert_changed: { label: 'Certificate changed', dot: 'bg-trash' },
  error: { label: 'Error', dot: 'bg-trash' },
};

const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

export async function load() {
  try {
    accounts.list = await accountsApi.list();
    accounts.loaded = true;
    accounts.error = '';
  } catch (e) {
    accounts.error = message(e);
  }
}

export async function loadPresets() {
  try {
    accounts.presets = await accountsApi.listPresets();
    accounts.presetsError = '';
  } catch (e) {
    accounts.presetsError = message(e);
  }
}

const put = (a: accountsApi.Account) => {
  accounts.list = accounts.list.map((x) => (x.id === a.id ? a : x));
};

// Only accounts already listed: an event that trails a removal must not bring the row back.
subscribe('account.status', (a) => put(a as accountsApi.Account));

/** The sentence shown after a connection test passes. */
export function testSummary(r: accountsApi.TestResult) {
  const special = r.folders.filter((f) => f.special_use).map((f) => f.special_use.slice(1));
  return (
    `Connected. ${r.folders.length} folders found` +
    (special.length ? `; ${new Intl.ListFormat('en-GB').format(special)} detected.` : '.') +
    (r.idle ? ' Push (IDLE) supported.' : ' No push here: new mail is checked for once a minute.') +
    (r.can_move ? '' : ' This server cannot move mail: rules can flag and mark it only.')
  );
}

/** Saves a mailbox. The password goes to the API and nowhere else. Throws when the daemon refuses. */
export async function connect(c: accountsApi.AccountInput) {
  const a = await accountsApi.create(c);
  accounts.list.push(a);
  flash(a.label + ' is connected');
  return a;
}

/** The folder names a mailbox can watch; a failure is shown as a toast and none are returned. */
export async function folderNames(id: number) {
  try {
    return (await accountsApi.folders(id)).map((f) => f.name);
  } catch (e) {
    flash(message(e));
    return [];
  }
}

/** Saves edits to a mailbox. A new password goes to the API and nowhere else. Throws when the daemon refuses. */
export async function edit(id: number, p: Pick<accountsApi.AccountPatch, 'label' | 'watch_folder' | 'password' | 'shared'>) {
  const a = await accountsApi.patch(id, p);
  put(a);
  flash(a.label + ' updated');
}

/** Runs one row action; a failure is shown as a toast and the list stays as it was. */
async function act(run: () => Promise<void>) {
  try {
    await run();
  } catch (e) {
    flash(message(e));
  }
}

export const reconnect = (id: number) =>
  act(async () => {
    const a = await accountsApi.reconnect(id);
    put(a);
    flash('Reconnecting ' + a.label);
  });

/** Checks a stored mailbox without disturbing it and says how it went; its status stays as it was. */
export const test = (id: number) => act(async () => flash(testSummary(await accountsApi.testStored(id))));

/**
 * Asks the mailbox's server for the certificate it presents now, on a connection of its own, so
 * the person can check it before accepting it. Resolves with it and the daemon's reason, or with
 * nothing when there is none to accept: the server takes the accepted one again (then the mailbox
 * reconnects), or the check failed (said in a toast).
 */
export async function checkCert(id: number): Promise<{ cert: accountsApi.ServerCert; message: string } | undefined> {
  try {
    await accountsApi.testStored(id);
  } catch (e) {
    if (e instanceof ApiError && e.cert) return { cert: e.cert, message: e.message };
    flash(message(e));
    return;
  }
  await reconnect(id);
}

/** Trusts this certificate, and only it, for the mailbox; the daemon reconnects with it. */
export const acceptCert = (id: number, fingerprint: string) =>
  act(async () => {
    const a = await accountsApi.patch(id, { cert_fingerprint: fingerprint });
    put(a);
    flash('Certificate accepted. Reconnecting ' + a.label);
  });

export const setPaused = (id: number, paused: boolean) =>
  act(async () => {
    const a = await accountsApi.patch(id, { paused });
    put(a);
    flash(a.label + (paused ? ' paused' : ' resumed'));
  });

export const remove = (id: number) =>
  act(async () => {
    const label = accounts.list.find((a) => a.id === id)?.label;
    await accountsApi.remove(id);
    accounts.list = accounts.list.filter((a) => a.id !== id);
    flash(label + ' removed');
  });
