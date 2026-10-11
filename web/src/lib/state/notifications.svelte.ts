import { subscribe } from '../api/events';
import * as notificationsApi from '../api/notifications';
import { settings } from './settings.svelte';

// Where rule notifications go. Only a build with a notifications module has any
// (settings.value.features.notifications); without one nothing here is fetched or shown.
export const notifications = $state<{ kinds: notificationsApi.Kind[]; destinations: notificationsApi.Destination[]; loaded: boolean; error: string }>({
  kinds: [],
  destinations: [],
  loaded: false,
  error: '',
});

export async function load() {
  if (!settings.value.features.notifications) return;
  try {
    const r = await notificationsApi.list();
    notifications.kinds = r.kinds;
    notifications.destinations = r.destinations;
    notifications.loaded = true;
    notifications.error = '';
  } catch (e) {
    notifications.error = (e as Error).message;
  }
}

/** Adds a destination; resolves it with its signing secret, which is never sent again (null for Slack). */
export async function create(input: notificationsApi.DestinationInput) {
  const created = await notificationsApi.create(input);
  notifications.destinations = [...notifications.destinations.filter((d) => d.id !== created.destination.id), created.destination];
  return created;
}

/** Removes a destination; the daemon also takes it out of every rule's `notify`. */
export async function remove(id: number) {
  await notificationsApi.remove(id);
  notifications.destinations = notifications.destinations.filter((d) => d.id !== id);
}

// A destination was added or removed elsewhere, or a delivery was recorded.
subscribe('notifications.changed', load);
