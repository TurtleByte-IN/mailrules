import * as sendersApi from '../api/senders';
import { rules } from './rules.svelte';
import { flash } from './toast.svelte';

export const senders = $state<{ list: sendersApi.Sender[]; rules: sendersApi.SenderRule[]; sort: sendersApi.Sort }>({
  list: [],
  rules: [],
  sort: 'volume',
});

export async function load() {
  const page = await sendersApi.list(senders.sort);
  senders.list = page.senders;
  senders.rules = page.rules;
}

export async function setSort(sort: sendersApi.Sort) {
  senders.sort = sort;
  await load();
}

/** Senders whose name or address contains the query. */
export function search(list: sendersApi.Sender[], query: string) {
  const q = query.trim().toLowerCase();
  return q ? list.filter((s) => (s.name + ' ' + s.address).toLowerCase().includes(q)) : list;
}

// The routing select speaks one value: 'auto', 'keep', 'trash' or a rule id.
const toRouting = (r?: sendersApi.SenderRule) =>
  !r ? 'auto' : r.verdict === 'keep' ? 'keep' : r.verdict === 'block' ? 'trash' : (r.ruleId ?? 'auto');

export const routingOf = (s: sendersApi.Sender) =>
  toRouting(
    senders.rules.find((r) => r.value === (r.type === 'domain' ? sendersApi.domainOf(s.address) : s.address)),
  );

const routeName = (routing: string) =>
  routing === 'keep'
    ? 'Keep in Inbox'
    : routing === 'trash'
      ? 'Trash'
      : (rules.list.find((r) => r.id === routing)?.name ?? routing);

export const targetOf = (r: sendersApi.SenderRule) => routeName(toRouting(r));

export async function setRouting(s: sendersApi.Sender, routing: string) {
  const domain = sendersApi.domainOf(s.address);
  if (routing === 'auto') await sendersApi.remove('domain', domain);
  else if (routing === 'keep' || routing === 'trash')
    await sendersApi.put('domain', domain, { verdict: routing === 'keep' ? 'keep' : 'block', ruleId: null });
  else await sendersApi.put('domain', domain, { verdict: 'route', ruleId: routing });
  await load();
  flash(routing === 'auto' ? s.name + ' goes back to your rules' : 'Mail from ' + s.name + ': ' + routeName(routing));
}

export async function unsubscribe(s: sendersApi.Sender) {
  await sendersApi.unsubscribe(s.address);
  await load();
  flash('Unsubscribed from ' + s.name + ' via List-Unsubscribe. Stragglers go to Trash.');
}

export async function forget(r: sendersApi.SenderRule) {
  await sendersApi.remove(r.type, r.value);
  await load();
}
