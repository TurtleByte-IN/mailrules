import type { ActivityItem } from '../api/activity';
import { subscribe } from '../api/events';
import * as reviewApi from '../api/review';
import { failed, outcome, upsert } from './activity.svelte';
import { badges } from './badges.svelte';
import { flash } from './toast.svelte';

export const review = $state({
  list: [] as ActivityItem[],
  /** Cursor of the next page; null at the end of the queue. */
  next: null as string | null,
  /** The size of the whole queue, loaded or not. */
  total: 0,
  /** False until the first page has arrived. */
  loaded: false,
  error: '',
});

// The nav count is the size of the whole queue; every change to it goes through here.
function set(list: ActivityItem[], total: number) {
  review.list = list;
  review.total = badges['/review'] = total;
}

const drop = (id: number) => {
  if (review.list.some((x) => x.id === id)) set(review.list.filter((x) => x.id !== id), review.total - 1);
};

export async function load() {
  try {
    const page = await reviewApi.list();
    set(page.items, page.total);
    review.next = page.next_cursor;
    review.loaded = true;
    review.error = '';
  } catch (e) {
    review.error = e instanceof Error ? e.message : String(e);
  }
}

export async function loadMore() {
  try {
    const page = await reviewApi.list(review.next);
    set([...review.list, ...page.items.filter((i) => !review.list.some((x) => x.id === i.id))], page.total);
    review.next = page.next_cursor;
  } catch (e) {
    failed(e);
  }
}

/** ruleId null means keep in Inbox. */
export async function resolve(id: number, ruleId: number | null, always: boolean) {
  try {
    const { item } = await reviewApi.resolve(id, { rule_id: ruleId, always_for_sender: always });
    drop(id);
    // The daemon also sends this row as message.processed; applying it twice is harmless.
    upsert(item);
    flash(ruleId === null ? 'Kept in Inbox' : 'Done: ' + outcome(item));
  } catch (e) {
    failed(e);
  }
}

subscribe('message.review', (item) => {
  if (!review.list.some((x) => x.id === item.id)) set([item, ...review.list], review.total + 1);
});
// Settled from the feed or from another window: it is no longer waiting.
subscribe('message.processed', (item) => {
  if (item.state !== 'review') drop(item.id);
});
