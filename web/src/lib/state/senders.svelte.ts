import { folders as listFolders } from '../api/accounts';
import * as sendersApi from '../api/senders';
import { rules } from './rules.svelte';
import { flash } from './toast.svelte';

type Sender = sendersApi.Sender;

export const senders = $state<{
  status: 'loading' | 'ready' | 'error';
  error: string;
  list: Sender[];
  /** Cursor of the next page of the list; null at the end. */
  next: string | null;
  /** Senders whose rule MailRules learned. */
  learned: Sender[];
  sort: sendersApi.Sort;
  q: string;
  /** Folders a sender can be filed in: every mailbox's, by name, Inbox left out. */
  folders: string[];
}>({ status: 'loading', error: '', list: [], next: null, learned: [], sort: 'volume', q: '', folders: [] });

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
    senders.status = 'error';
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

// The mailboxes whose folders were asked for last, so a late answer for an older set is dropped.
let foldersFor = '';

/**
 * Lists the folders of these mailboxes. A sender's folder is a name used on every mailbox,
 * as a rule's move is, so the names are merged; a mailbox that lacks one gets it on the first
 * live move. A mailbox whose list cannot be read adds nothing.
 */
export async function loadFolders(accountIds: number[]) {
  const mine = (foldersFor = accountIds.join(','));
  const lists = await Promise.all(accountIds.map((id) => listFolders(id).catch(() => [])));
  if (mine !== foldersFor) return;
  const names = new Set(lists.flat().map((f) => f.name));
  senders.folders = [...names].filter((n) => n.toUpperCase() !== 'INBOX').sort((a, b) => a.localeCompare(b));
}

export const nameOf = (s: Sender) => s.name || s.value;

const MOVE = 'move:';

// The routing select speaks one value: 'auto', 'keep', 'trash', 'move:<folder>' or a rule id.
export const routingOf = (s: Sender) =>
  s.verdict === 'keep'
    ? 'keep'
    : s.verdict === 'block'
      ? 'trash'
      : s.verdict === 'move'
        ? MOVE + (s.folder ?? '')
        : s.verdict === 'route'
          ? String(s.rule_id)
          : 'auto';

/** The folder a 'move:<folder>' routing names; null for any other routing. */
export const folderOf = (routing: string) => (routing.startsWith(MOVE) ? routing.slice(MOVE.length) : null);

/** The routing value that files a sender's mail in this folder. */
export const moveTo = (folder: string) => MOVE + folder;

const routeName = (routing: string) =>
  routing === 'keep'
    ? 'Keep in Inbox'
    : routing === 'trash'
      ? 'Trash'
      : folderOf(routing) !== null
        ? 'Move to ' + folderOf(routing)
        : (rules.list.find((r) => String(r.id) === routing)?.name ?? routing);

export const targetOf = (s: Sender) => routeName(routingOf(s));

function toPut(routing: string): sendersApi.SenderPut {
  const folder = folderOf(routing);
  if (folder !== null) return { verdict: 'move', folder };
  return routing === 'keep' ? { verdict: 'keep' } : routing === 'trash' ? { verdict: 'block' } : { verdict: 'route', rule_id: Number(routing) };
}

// Whatever the user just set or removed is no longer a learned rule.
function replace(s: Sender) {
  senders.list = senders.list.map((x) => (same(x, s) ? s : x));
  senders.learned = senders.learned.filter((x) => !same(x, s));
}

// DELETE answers 204 with no body: the sender is what it was, minus its rule.
async function removeRule(s: Sender) {
  await sendersApi.remove(s);
  replace({ ...s, verdict: null, rule_id: null, folder: null, source: null, hits: 0 });
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
