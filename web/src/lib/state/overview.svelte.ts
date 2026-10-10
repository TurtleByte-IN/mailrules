import type { Account } from '../api/accounts';
import { clock, day } from '../format';
import * as activityApi from '../api/activity';
import { subscribe } from '../api/events';

export const overview = $state({
  /** Null until today's numbers have arrived, so zeros are never shown for "not loaded". */
  stats: null as activityApi.StatsSummary | null,
  latest: [] as activityApi.ActivityItem[],
  error: '',
});

export async function load() {
  try {
    const [stats, page] = await Promise.all([activityApi.summary(), activityApi.list({ limit: 5 })]);
    overview.stats = stats;
    overview.latest = page.items;
    overview.error = '';
  } catch (e) {
    overview.error = e instanceof Error ? e.message : String(e);
  }
}

// Only once the screen has been opened: until then nobody is looking at these numbers.
subscribe('message.processed', () => overview.stats && load());
subscribe('action.undone', () => overview.stats && load());

/** Where today's mail went, as the four parts of the bar. The daemon's four numbers add up to the emails processed;
 * `outcome` is the Activity filter that lists the same emails. */
export function split(went: activityApi.StatsSummary['went']) {
  const parts = [
    { label: 'Sorted', outcome: 'sorted', n: went.sorted, fill: 'bg-ink' },
    { label: 'Left in Inbox', outcome: 'inbox', n: went.inbox, fill: 'bg-idle' },
    { label: 'Needs review', outcome: 'review', n: went.review, fill: 'bg-attention' },
    { label: 'Trashed', outcome: 'trashed', n: went.trashed, fill: 'bg-signal' },
  ];
  const total = parts.reduce((a, p) => a + p.n, 0);
  return parts.map((p) => ({ ...p, pct: total ? (p.n / total) * 100 : 0 }));
}

/** "Jev 31 calls · Haiku 3": calls per model, a model counted once whatever it was asked for. */
export function callsLine(models: activityApi.StatsSummary['calls_by_model']) {
  const by = new Map<string, number>();
  for (const m of models) by.set(m.model, (by.get(m.model) ?? 0) + m.calls);
  return [...by].map(([model, calls], i) => model + ' ' + calls + (i ? '' : calls === 1 ? ' call' : ' calls')).join(' · ') || 'no model calls';
}

/** The line at the top: one sentence on whether every mailbox is connected. */
export function healthLine(list: Account[]) {
  const bad = list.filter((a) => a.status !== 'live').length;
  if (!list.length) return { ok: false, text: 'No mailbox connected yet' };
  if (bad) return { ok: false, text: bad + (bad === 1 ? ' mailbox needs attention' : ' mailboxes need attention') };
  return { ok: true, text: list.length === 1 ? 'Your mailbox is live' : 'All ' + list.length + ' mailboxes live' };
}

/**
 * What a mailbox's row says and offers. `go` sends the user to Mailboxes; `sign-in` to the provider's sign-in
 * for a one-click mailbox (`reconnectURL`); the other actions run in place.
 */
export function health(a: Account): { detail: string; chip: string; action?: 'test' | 'reconnect' | 'resume' | 'go' | 'sign-in' | 'wait'; label?: string } {
  switch (a.status) {
    case 'live':
      return {
        detail: (a.capabilities.includes('IDLE') ? 'IDLE connected' : 'Checked once a minute') + (a.last_mail_at ? ' · last email ' + day(a.last_mail_at) + ', ' + clock(a.last_mail_at) : ''),
        chip: 'chip-live',
        action: 'test',
        label: 'Test',
      };
    case 'new':
      return { detail: 'Logging in and syncing folders…', chip: 'chip-neutral', action: 'wait', label: 'Working…' };
    case 'reconnecting':
      return { detail: 'Connection dropped · nothing missed, catch-up runs on reconnect', chip: 'chip-review', action: 'reconnect', label: 'Reconnect now' };
    case 'paused':
      return { detail: 'Paused · new mail is not sorted', chip: 'chip-neutral', action: 'resume', label: 'Resume' };
    case 'auth_failed':
      return { detail: a.last_error || 'Login rejected.', chip: 'chip-trash', action: 'go', label: 'Fix sign-in' };
    case 'reconnect_needed':
      return { detail: 'Sign-in revoked or expired. Reconnect to sign in again.', chip: 'chip-trash', action: 'sign-in', label: 'Reconnect' };
    case 'cert_changed':
      return { detail: "The server's certificate changed.", chip: 'chip-trash', action: 'go', label: 'Check certificate' };
    case 'error':
      return { detail: a.last_error || 'Could not connect.', chip: 'chip-trash', action: 'reconnect', label: 'Reconnect now' };
  }
}
