import * as accountsApi from '../api/accounts';
import { flash } from './toast.svelte';

export const accounts = $state<{ list: accountsApi.Account[]; presets: accountsApi.Preset[] }>({
  list: [],
  presets: [],
});

export async function load() {
  accounts.list = await accountsApi.list();
}

export async function loadPresets() {
  accounts.presets = await accountsApi.listPresets();
}

/** The sentence shown after a connection test passes. */
export const testSummary = (r: accountsApi.TestResult) =>
  `Connected. ${r.folders} folders found; ${new Intl.ListFormat('en-GB').format(r.special)} detected.` +
  (r.idle ? ' Push (IDLE) supported.' : '');

/** Saves a tested mailbox. The password goes to the API and nowhere else. */
export async function connect(c: accountsApi.Credentials) {
  const a = await accountsApi.create(c);
  accounts.list.push(a);
  flash(a.email + ' is live');
}

export async function reconnect(id: string) {
  const a = await accountsApi.reconnect(id);
  accounts.list = accounts.list.map((x) => (x.id === id ? a : x));
  flash(a.email + ' reconnected');
}

export async function test(id: string) {
  flash(testSummary(await accountsApi.testSaved(id)));
}

export async function remove(id: string) {
  const email = accounts.list.find((a) => a.id === id)?.email;
  await accountsApi.remove(id);
  accounts.list = accounts.list.filter((a) => a.id !== id);
  flash(email + ' removed');
}
