// DEMO: GET /api/senders?sort=volume and PUT DELETE /api/senders/{type}/{value} are not in
// api/openapi.yaml yet (backend M9). POST /api/senders/unsubscribe is in no plan yet; see
// unsubscribe() below.
import { fake } from './demo';

export type Sort = 'volume' | 'unread';

export interface Sender {
  address: string;
  name: string;
  /** Emails in the last 30 days. */
  count: number;
  /** Percent of those the user opened. */
  readPct: number;
  /** True when the sender's mail carries a List-Unsubscribe header. */
  list: boolean;
  unsubscribed: boolean;
}

/** One row of sender_rules in docs/backend-plan.md → Data model. */
export interface SenderRule {
  type: 'address' | 'domain';
  value: string;
  verdict: 'route' | 'keep' | 'block';
  /** Set when verdict is route. */
  ruleId: string | null;
  source: 'user' | 'learned';
  /** Why a learned rule exists, as the prototype words it. */
  why?: string;
}

export interface Senders {
  senders: Sender[];
  rules: SenderRule[];
}

let senders: Sender[] = [
  { address: 'hello@weeklygodigest.dev', name: 'Weekly Go Digest', count: 30, readPct: 12, list: true, unsubscribed: false },
  { address: 'noreply@swiggy.in', name: 'Swiggy', count: 64, readPct: 41, list: true, unsubscribed: false },
  { address: 'notifications@linkedin.com', name: 'LinkedIn', count: 88, readPct: 6, list: true, unsubscribed: false },
  { address: 'shipment-tracking@amazon.in', name: 'Amazon.in', count: 47, readPct: 58, list: false, unsubscribed: false },
  { address: 'noreply@medium.com', name: 'Medium Daily Digest', count: 31, readPct: 3, list: true, unsubscribed: false },
  { address: 'alerts@hdfcbank.net', name: 'HDFC Bank', count: 22, readPct: 91, list: false, unsubscribed: false },
  { address: 'offers@myntra.com', name: 'Myntra', count: 53, readPct: 2, list: true, unsubscribed: false },
  { address: 'rahul@mehta.studio', name: 'Rahul Mehta', count: 9, readPct: 100, list: false, unsubscribed: false },
];

let rules: SenderRule[] = [
  { type: 'domain', value: 'weeklygodigest.dev', verdict: 'route', ruleId: 'r5', source: 'user' },
  { type: 'domain', value: 'hdfcbank.net', verdict: 'keep', ruleId: null, source: 'user' },
  { type: 'domain', value: 'jobalerts.in', verdict: 'route', ruleId: 'r3', source: 'learned', why: 'you corrected 3 emails' },
  { type: 'domain', value: 'irctc.co.in', verdict: 'keep', ruleId: null, source: 'learned', why: 'you reviewed 2 emails' },
];

export const domainOf = (address: string) => address.split('@')[1];

const same = (r: SenderRule, type: SenderRule['type'], value: string) => r.type === type && r.value === value;

export function list(sort: Sort = 'volume') {
  const sorted = senders.slice().sort((a, b) => (sort === 'unread' ? a.readPct - b.readPct : b.count - a.count));
  return fake<Senders>({ senders: sorted, rules });
}

export function put(type: SenderRule['type'], value: string, verdict: Pick<SenderRule, 'verdict' | 'ruleId'>) {
  const rule: SenderRule = { type, value, ...verdict, source: 'user' };
  rules = [rule, ...rules.filter((r) => !same(r, type, value))];
  return fake(rule);
}

export function remove(type: SenderRule['type'], value: string) {
  rules = rules.filter((r) => !same(r, type, value));
  return fake(undefined);
}

/** One-click List-Unsubscribe. A sender with no rule yet gets a block rule for the stragglers. */
export function unsubscribe(address: string) {
  senders = senders.map((s) => (s.address === address ? { ...s, unsubscribed: true } : s));
  const domain = domainOf(address);
  if (!rules.some((r) => same(r, 'domain', domain))) {
    rules = [{ type: 'domain', value: domain, verdict: 'block', ruleId: null, source: 'user' }, ...rules];
  }
  return fake(undefined);
}
