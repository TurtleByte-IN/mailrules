import { describe, expect, it } from 'vitest';
import type { Account } from '../api/accounts';
import { callsLine, health, healthLine, split } from './overview.svelte';

const account = (over: Partial<Account> = {}): Account => ({
  id: 1, label: 'me@icloud.com', preset: 'icloud', host: 'imap.mail.me.com', port: 993, tls_mode: 'implicit', username: 'me@icloud.com', watch_folder: 'INBOX',
  status: 'live', last_error: '', last_event_at: 1000, last_mail_at: null, capabilities: ['IDLE'], can_move: true, folder_count: 14, created_at: 900, ...over,
});

describe('split', () => {
  it.each([
    ['a normal day', { sorted: 29, inbox: 6, review: 3, trashed: 2 }],
    ['nothing yet', { sorted: 0, inbox: 0, review: 0, trashed: 0 }],
    ['only review', { sorted: 0, inbox: 0, review: 5, trashed: 0 }],
  ])('%s', (_name, went) => {
    const parts = split(went);
    expect(parts.map((p) => p.n)).toEqual([went.sorted, went.inbox, went.review, went.trashed]);
    const total = went.sorted + went.inbox + went.review + went.trashed;
    expect(Math.round(parts.reduce((a, p) => a + p.pct, 0))).toBe(total ? 100 : 0);
  });
});

it('callsLine adds a model up across purposes', () => {
  const m = (model: string, purpose: 'decide' | 'escalate', calls: number) => ({ provider: model, model, purpose, calls, tokens_in: 0, tokens_out: 0, cost_usd: 0 });
  expect(callsLine([m('jev', 'decide', 30), m('jev', 'escalate', 1), m('haiku', 'escalate', 3)])).toBe('jev 31 calls · haiku 3');
  expect(callsLine([])).toBe('no model calls');
});

it.each([
  [[], false, 'No mailbox connected yet'],
  [[account()], true, 'Your mailbox is live'],
  [[account(), account({ id: 2 })], true, 'All 2 mailboxes live'],
  [[account(), account({ id: 2, status: 'auth_failed' })], false, '1 mailbox needs attention'],
  [[account({ status: 'paused' }), account({ id: 2, status: 'error' })], false, '2 mailboxes need attention'],
])('healthLine(%#)', (list, ok, text) => expect(healthLine(list)).toEqual({ ok, text }));

it.each([
  ['live', undefined],
  ['new', 'wait'],
  ['reconnecting', 'reconnect'],
  ['paused', 'resume'],
  ['auth_failed', 'go'],
  ['error', 'reconnect'],
] as const)('a %s mailbox offers %s', (status, action) => expect(health(account({ status })).action).toBe(action));

it('shows the daemon\'s reason for a failed sign-in', () =>
  expect(health(account({ status: 'auth_failed', last_error: 'App password revoked.' })).detail).toBe('App password revoked.'));
