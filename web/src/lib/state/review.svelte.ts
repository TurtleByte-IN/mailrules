import { subscribe } from '../api/events';
import * as reviewApi from '../api/review';
import { upsert } from './activity.svelte';
import { badges } from './badges.svelte';
import { flash } from './toast.svelte';

export const review = $state<{ list: reviewApi.ReviewItem[] }>({ list: [] });

// The nav count is the queue length; every change to the queue goes through here.
function set(list: reviewApi.ReviewItem[]) {
  review.list = list;
  badges['/review'] = list.length;
}

export async function load() {
  set(await reviewApi.list());
}

/** ruleId null means keep in Inbox. */
export async function resolve(id: string, ruleId: string | null, always: boolean) {
  const row = await reviewApi.resolve(id, ruleId, always);
  set(review.list.filter((x) => x.id !== id));
  // The daemon also sends this row as message.processed; applying it twice is harmless.
  upsert(row);
  flash(row.ruleId ? 'Done: ' + row.outcome : 'Kept in Inbox');
}

subscribe('message.review', (item) => set([item, ...review.list.filter((x) => x.id !== item.id)]));
