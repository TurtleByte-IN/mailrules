import { notBuilt } from '../api/client';
import * as sendersApi from '../api/senders';
import { rules } from './rules.svelte';
import { flash } from './toast.svelte';

type Sender = sendersApi.Sender;

export const senders = $state<{
  status: 'loading' | 'ready' | 'not_built' | 'error';
  error: string;
  list: Sender[];
  /** Cursor of the next page of the list; null at the end. */
  next: string | null;
  /** Senders whose rule MailRules learned. */
  learned: Sender[];
  sort: sendersApi.Sort;
  q: string;
}>({ status: 'loading', error: '', list: [], next: null, learned: [], sort: 'volume', q: '' });

const fail = (e: unknown) => flash((e as Error).message);
const same = (a: Sender, b: Sender) => a.type === b.type && a.value === b.value;

// Search asks the server on every keystroke, so a slow earlier answer must not replace a later one.
let latest = 0;

async function fetchList() {
  const mine = ++latest;
  const page = await sendersApi.list({ sort: senders.sort, q: senders.q });
  if (mine !== latest) return;
  senders.list = page.items;
  senders.next = page.next_cursor;
}

export async function load() {
  try {
    // ponytail: the learned list is its first 100; page it if anyone learns more.
    const [, learned] = await Promise.all([fetchList(), sendersApi.list({ source: 'learned', limit: 100 })]);
    senders.learned = learned.items;
    senders.status = 'ready';
  } catch (e) {
    senders.status = notBuilt(e) ? 'not_built' : 'error';
    senders.error = (e as Error).message;
  }
}

export function setSort(sort: sendersApi.Sort) {
  senders.sort = sort;
  return fetchList().catch(fail);
}

export function setQuery(q: string) {
  senders.q = q.trim();
  return fetchList().catch(fail);
}

export async function more() {
  const mine = latest;
  try {
    const page = await sendersApi.list({ sort: senders.sort, q: senders.q, cursor: senders.next ?? undefined });
    if (mine !== latest) return;
    senders.list.push(...page.items);
    senders.next = page.next_cursor;
  } catch (e) {
    fail(e);
  }
}

export const nameOf = (s: Sender) => s.name || s.value;

// The routing select speaks one value: 'auto', 'keep', 'trash' or a rule id.
export const routingOf = (s: Sender) =>
  s.verdict === 'keep' ? 'keep' : s.verdict === 'block' ? 'trash' : s.verdict === 'route' ? String(s.rule_id) : 'auto';

const routeName = (routing: string) =>
  routing === 'keep'
    ? 'Keep in Inbox'
    : routing === 'trash'
      ? 'Trash'
      : (rules.list.find((r) => String(r.id) === routing)?.name ?? routing);

export const targetOf = (s: Sender) => routeName(routingOf(s));

const toPut = (routing: string): sendersApi.SenderPut =>
  routing === 'keep' ? { verdict: 'keep' } : routing === 'trash' ? { verdict: 'block' } : { verdict: 'route', rule_id: Number(routing) };

// Whatever the user just set or removed is no longer a learned rule.
function replace(s: Sender) {
  senders.list = senders.list.map((x) => (same(x, s) ? s : x));
  senders.learned = senders.learned.filter((x) => !same(x, s));
}

// DELETE answers 204 with no body: the sender is what it was, minus its rule.
async function removeRule(s: Sender) {
  await sendersApi.remove(s);
  replace({ ...s, verdict: null, rule_id: null, source: null, hits: 0 });
}

/** False when the daemon refused, so the screen can put the select back. */
export async function setRouting(s: Sender, routing: string) {
  try {
    if (routing === 'auto') await removeRule(s);
    else replace(await sendersApi.put(s, toPut(routing)));
    flash(routing === 'auto' ? nameOf(s) + ' goes back to your rules' : 'Mail from ' + nameOf(s) + ': ' + routeName(routing));
    return true;
  } catch (e) {
    fail(e);
    return false;
  }
}

export const forget = (s: Sender) => removeRule(s).catch(fail);
