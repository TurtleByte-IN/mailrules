// The one EventSource for the app (GET /api/events). State modules subscribe by event
// name and patch their own data; see docs/frontend-plan.md → SSE events to state.
// Nothing calls open() yet: every resource that listens is still on demo data and the
// daemon does not serve the stream. The shell calls it after sign-in once it does.
import type { ActivityRow } from './activity';
import type { ReviewItem } from './review';

export interface Events {
  'message.processed': ActivityRow;
  'message.review': ReviewItem;
  'action.undone': { actionId: string };
  // Payloads below belong to resources that have not defined them yet.
  'account.status': unknown;
  'batch.progress': unknown;
  'rules.changed': unknown;
  'usage.updated': unknown;
}

// A record, not an array, so the compiler fails when an event is added above and not here.
const names: Record<keyof Events, true> = {
  'message.processed': true,
  'message.review': true,
  'action.undone': true,
  'account.status': true,
  'batch.progress': true,
  'rules.changed': true,
  'usage.updated': true,
};

const handlers = new Map<string, Set<(data: never) => void>>();
let source: EventSource | undefined;

/** Listen for one event; returns the function that stops listening. */
export function subscribe<K extends keyof Events>(name: K, handler: (data: Events[K]) => void) {
  const set = handlers.get(name) ?? new Set();
  handlers.set(name, set);
  set.add(handler);
  return () => void set.delete(handler);
}

/** Hand one event to its subscribers. open() feeds this; tests call it directly. */
export function dispatch<K extends keyof Events>(name: K, data: Events[K]) {
  for (const h of handlers.get(name) ?? []) h(data as never);
}

export function open() {
  if (source) return;
  source = new EventSource('/api/events');
  for (const name of Object.keys(names) as (keyof Events)[]) {
    source.addEventListener(name, (e) => dispatch(name, JSON.parse((e as MessageEvent).data)));
  }
}

export function close() {
  source?.close();
  source = undefined;
}
