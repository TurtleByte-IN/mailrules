// Test support: payloads shaped like api/openapi.yaml, and a fetch stub that records calls.
import { vi } from 'vitest';
import type { ActivityItem, MessageAction, MessageDetail, StatsSummary } from '../api/activity';

export const decision = (over: Partial<NonNullable<ActivityItem['decision']>> = {}): NonNullable<ActivityItem['decision']> => ({
  id: 11, stage: 'decider', rule_id: 3, rule_name: 'Recruiters', confidence: 0.91, reason: 'Recruiter outreach; you have never replied to this sender',
  model: 'jev', tokens_in: 400, tokens_out: 12, cost_usd: 0.00002, latency_ms: 380, created_at: 1000, ...over,
});

export const action = (over: Partial<MessageAction> = {}): MessageAction => ({
  id: 21, message_id: 1, account_id: 1, decision_id: 11, batch_id: 5, kind: 'move', folder: 'Jobs', from_folder: 'INBOX', to_folder: 'Jobs',
  status: 'done', error: '', created_at: 1000, undone_at: null, ...over,
});

export const item = (over: Partial<ActivityItem> = {}): ActivityItem => ({
  id: 1, account_id: 1, from: 'priya@talentbridge.in', from_domain: 'talentbridge.in', subject: 'Senior backend role', snippet: 'Hi, I came across your profile',
  received_at: 1000, created_at: 1001, has_attachment: false, state: 'acted', decision: decision(), actions: [action()], undoable: true, correction: null, ...over,
});

export const inReview = (id: number): ActivityItem =>
  item({ id, state: 'review', decision: decision({ stage: 'fallback', confidence: 0.58, reason: 'Could be Recruiters or Newsletters' }), actions: [], undoable: false });

export const detail = (over: Partial<MessageDetail> = {}): MessageDetail => ({
  ...item(), to: ['me@icloud.com'], list_id: '', size: 2048, signals: { bulk: false, noreply: false, is_contact: false, replied_before: false, dmarc: 'pass' },
  folder: 'INBOX', current_folder: 'Jobs', attempts: 0, next_attempt_at: null,
  trace: [
    { kind: 'decider', label: 'Decision model', detail: 'Recruiter outreach', rule_id: 3, rule_name: 'Recruiters', confidence: 0.91, model: 'jev', tokens_in: 400, tokens_out: 12, cost_usd: 0.00002, latency_ms: 380, status: '', at: 1000, active: true, candidates: [] },
    { kind: 'action', label: 'Action', detail: 'Moved to Jobs', rule_id: null, rule_name: '', confidence: null, model: '', tokens_in: 0, tokens_out: 0, cost_usd: 0, latency_ms: 0, status: 'done', at: 1000, active: false, candidates: [] },
  ],
  ...over,
});

export const stats: StatsSummary = {
  range: 'day', since: 0, counts: { processed: 40, sorted: 31, trashed: 2, review: 3 }, went: { sorted: 29, inbox: 6, review: 3, trashed: 2 }, decided_without_model: 0.78, cost_usd: 0.01,
  calls_by_model: [{ provider: 'jev', model: 'jev', purpose: 'decide', calls: 31, tokens_in: 12000, tokens_out: 400, cost_usd: 0.01 }],
  top_rules: [{ rule_id: 3, rule_name: 'Recruiters', hits: 9 }],
  quiet_rules: 0,
  accounts: [{ account_id: 1, label: 'me@icloud.com', preset: 'icloud', username: 'me@icloud.com', folder_count: 14, status: 'live', last_event_at: 1000, last_error: '' }],
};

export const error = (code: string, message: string) => ({ error: { code, message } });

export type Call = { call: string; body?: unknown };

/** Stub fetch. reply gets "METHOD /api/path?query" and the parsed body; the returned array collects every call. */
export function serve(reply: (call: string, body: unknown) => [status: number, body?: unknown]) {
  const calls: Call[] = [];
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const call = `${init.method} ${url}`;
      const body = init.body ? JSON.parse(init.body as string) : undefined;
      calls.push(body === undefined ? { call } : { call, body });
      const [status, json] = reply(call, body);
      return new Response(json === undefined ? null : JSON.stringify(json), { status });
    }),
  );
  return calls;
}
