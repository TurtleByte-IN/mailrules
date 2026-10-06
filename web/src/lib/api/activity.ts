// DEMO: GET /api/activity, GET /api/messages/{id}, POST /api/messages/{id}/correct,
// POST /api/actions/{id}/undo and POST /api/rules/{id}/undo?since= are not in
// api/openapi.yaml yet (backend M7).
import { fake, fakeId } from './demo';
import { list as listRules, type Rule } from './rules';

/** How the row reads in the feed: sorted by a rule, sent to Trash, left alone, or waiting for the user. */
export type Kind = 'ok' | 'trash' | 'none' | 'review';

/** decisions.stage in the data model, plus the two stages a person decides at. */
export type Stage = 'sender' | 'condition' | 'decider' | 'fallback' | 'none' | 'corrected' | 'reviewed';

export interface TraceStep {
  label: string;
  detail: string;
  /** Tokens, cost and latency, already worded. */
  meta: string;
  /** The step that decided. */
  active: boolean;
}

export interface ActivityRow {
  /** Message id. */
  id: string;
  /** Unix seconds. */
  receivedAt: number;
  sender: string;
  domain: string;
  accountId: string;
  subject: string;
  /** One sentence: why this happened. */
  reason: string;
  ruleId: string | null;
  rule: string;
  kind: Kind;
  stage: Stage;
  model?: string;
  confidence?: number;
  outcome: string;
  /** The action Undo reverses; null when nothing in the mailbox changed. */
  actionId: string | null;
  undone: boolean;
}

export interface Message extends ActivityRow {
  from: string;
  /** Plain text, kept for the retention period. Never HTML. */
  snippet: string;
  trace: TraceStep[];
}

const now = Math.floor(Date.now() / 1000);
const ago = (minutes: number) => now - minutes * 60;
const step = (label: string, detail: string, meta = '', active = false): TraceStep => ({ label, detail, meta, active });

/** Exported for review.ts, which re-decides these same demo messages. */
export const messages: Message[] = [
  {
    id: 'a1', receivedAt: ago(4), sender: 'Swiggy', from: 'Swiggy <noreply@swiggy.in>', domain: 'swiggy.in', accountId: 'acc1',
    subject: 'Your order from Meghana Foods is on the way', reason: 'Sender domain swiggy.in matches Food orders',
    ruleId: 'r1', rule: 'Food orders', kind: 'ok', stage: 'condition', outcome: 'Moved to Food · read', actionId: 'act1', undone: false,
    snippet: 'Your order is out for delivery. Track your rider live in the app. Estimated arrival 9:05 AM.',
    trace: [
      step('Sender rules', 'No sender rule for swiggy.in', '0 tokens'),
      step('Conditions', 'from_domain in swiggy.in, zomato.com matched Food orders', '0 tokens · 1 ms', true),
      step('Action', 'Moved to Food and marked read, 1.4 s after arrival'),
    ],
  },
  {
    id: 'a2', receivedAt: ago(8), sender: 'Priya, TalentBridge', from: 'Priya <priya@talentbridge.in>', domain: 'talentbridge.in', accountId: 'acc1',
    subject: 'Senior backend role, Go and Kubernetes, remote', reason: 'Recruiter outreach; you have never replied to this sender',
    ruleId: 'r3', rule: 'Recruiters', kind: 'ok', stage: 'decider', model: 'jev', confidence: 0.91, outcome: 'Moved to Jobs', actionId: 'act2', undone: false,
    snippet: "Hi, I came across your profile and thought you'd be a great fit for a Senior Backend Engineer role at a Series B fintech. The stack is Go, Postgres and Kubernetes...",
    trace: [
      step('Sender rules', 'No rule for talentbridge.in', '0 tokens'),
      step('Conditions', 'Recruiters passed its exception: no replies to this sender', '0 tokens'),
      step('Decision model (Jev)', 'Recruiters 0.91 · Newsletters 0.06 · none 0.03', '412 tokens · $0.00002 · 380 ms', true),
      step('Action', 'Moved to Jobs, 2.1 s after arrival'),
    ],
  },
  {
    id: 'a3', receivedAt: ago(15), sender: 'HDFC Bank Alerts', from: 'HDFC Bank Alerts <alerts@hdfc-secure-verify.co>', domain: 'hdfc-secure-verify.co', accountId: 'acc1',
    subject: 'Your account will be blocked today, verify KYC', reason: 'Look-alike domain, DMARC failed, urgent KYC demand',
    ruleId: 'r4', rule: 'Scams', kind: 'trash', stage: 'fallback', model: 'haiku', confidence: 0.97, outcome: 'Moved to Trash', actionId: 'act3', undone: false,
    snippet: 'Dear customer, your account will be suspended within 24 hours. Click here to complete KYC verification immediately...',
    trace: [
      step('Decision model (Jev)', 'Scams 0.68: below 0.75, asked for a second opinion', '398 tokens · $0.00002'),
      step('Fallback (Claude Haiku 4.5)', 'Scams 0.97: look-alike domain, failed DMARC, urgency', '1,712 tokens · $0.0019', true),
      step('Action', 'Moved to Trash (rule needs 0.90); restorable for 30 days'),
    ],
  },
  {
    id: 'a4', receivedAt: ago(23), sender: 'Weekly Go Digest', from: 'Weekly Go Digest <hello@weeklygodigest.dev>', domain: 'weeklygodigest.dev', accountId: 'acc2',
    subject: 'Issue 212: generics in practice', reason: 'Learned from 3 past decisions for this sender',
    ruleId: 'r5', rule: 'Newsletters', kind: 'ok', stage: 'sender', outcome: 'Moved to Reading · read', actionId: 'act4', undone: false,
    snippet: 'This week: when generics help and when an interface is still the better tool, plus five libraries worth a look.',
    trace: [
      step('Sender rules', 'Learned rule: weeklygodigest.dev goes to Newsletters', '0 tokens', true),
      step('Action', 'Moved to Reading and marked read'),
    ],
  },
  {
    id: 'a5', receivedAt: ago(30), sender: 'Amazon.in', from: 'Amazon.in <shipment-tracking@amazon.in>', domain: 'amazon.in', accountId: 'acc1',
    subject: 'Your package was delivered', reason: 'Delivery update for an order you placed',
    ruleId: 'r6', rule: 'Orders', kind: 'ok', stage: 'decider', model: 'jev', confidence: 0.88, outcome: 'Moved to Shopping', actionId: 'act5', undone: false,
    snippet: 'Your package was delivered to the front door. Order #404-1234567.',
    trace: [
      step('Decision model (Jev)', 'Orders 0.88 · Newsletters 0.09', '377 tokens · $0.00002', true),
      step('Action', 'Moved to Shopping'),
    ],
  },
  {
    id: 'a6', receivedAt: ago(36), sender: 'Rahul Mehta', from: 'Rahul Mehta <rahul@mehta.studio>', domain: 'mehta.studio', accountId: 'acc2',
    subject: 'Re: invoice for September', reason: 'A contact you reply to; no rule applies',
    ruleId: null, rule: 'No rule', kind: 'none', stage: 'none', model: 'jev', confidence: 0.94, outcome: 'Kept in Inbox', actionId: null, undone: false,
    snippet: 'Thanks, received the invoice. Payment goes out on Friday.',
    trace: [
      step('Decision model (Jev)', 'None of your rules: 0.94', '402 tokens · $0.00002', true),
      step('Action', 'Left in Inbox'),
    ],
  },
  {
    id: 'a7', receivedAt: ago(54), sender: 'Apple', from: 'Apple <noreply@email.apple.com>', domain: 'apple.com', accountId: 'acc1',
    subject: 'Your verification code', reason: 'Subject mentions a verification code',
    ruleId: 'r2', rule: 'Login codes', kind: 'ok', stage: 'condition', outcome: 'Kept · flagged', actionId: 'act7', undone: false,
    snippet: 'Your verification code is 482 913. It expires in 10 minutes.',
    trace: [
      step('Conditions', 'subject contains verification', '0 tokens', true),
      step('Action', 'Kept in Inbox and flagged'),
    ],
  },
  {
    id: 'a8', receivedAt: ago(61), sender: 'JobAlerts', from: 'JobAlerts <alerts@jobalerts.in>', domain: 'jobalerts.in', accountId: 'acc1',
    subject: '12 new jobs matching Backend Developer', reason: 'Could be Recruiters or Newsletters; left in your inbox',
    ruleId: null, rule: 'Needs review', kind: 'review', stage: 'fallback', model: 'haiku', confidence: 0.58, outcome: 'In Inbox', actionId: null, undone: false,
    snippet: '12 new jobs this week matching Backend Developer in Bengaluru and remote.',
    trace: [
      step('Decision model (Jev)', 'Recruiters 0.52 · Newsletters 0.41', '389 tokens'),
      step('Fallback (Claude Haiku 4.5)', 'Recruiters 0.58: still unsure', '1,640 tokens · $0.0018', true),
      step('Needs review', 'Left in Inbox for you to decide'),
    ],
  },
];

// The demo Rule carries no outcome wording; the daemon derives it from the rule's actions.
const outcomes: Record<string, string> = {
  r1: 'Moved to Food · read',
  r2: 'Kept · flagged',
  r3: 'Moved to Jobs',
  r4: 'Moved to Trash',
  r5: 'Moved to Reading · read',
  r6: 'Moved to Shopping',
};

export const row = ({ from: _from, snippet: _snippet, trace: _trace, ...r }: Message): ActivityRow => r;

export const findRule = async (id: string | null) => (id ? (await listRules()).find((r) => r.id === id) : undefined);

/** What the daemon does to a message when a person overrides the decision; no rule means keep in Inbox. */
export function redecide(m: Message, r: Rule | undefined, stage: 'corrected' | 'reviewed', reason: string, last: Omit<TraceStep, 'active'>) {
  Object.assign(m, {
    ruleId: r?.id ?? null,
    rule: r?.name ?? 'No rule',
    kind: r ? (r.trash ? 'trash' : 'ok') : 'none',
    stage,
    model: undefined,
    confidence: undefined,
    outcome: r ? (outcomes[r.id] ?? 'Moved to ' + r.name) : 'Kept in Inbox',
    reason,
    actionId: r ? fakeId('act') : null,
    undone: false,
  } satisfies Partial<Message>);
  m.trace = [...m.trace.map((t) => ({ ...t, active: false })), { ...last, active: true }];
}

export const list = () => fake(messages.map(row));

export const get = (id: string) => fake(messages.find((m) => m.id === id)!);

/** ruleId null puts the message back in the Inbox; always also saves a sender rule for its domain. */
export async function correct(id: string, ruleId: string | null, always: boolean) {
  const m = messages.find((x) => x.id === id)!;
  const r = await findRule(ruleId);
  redecide(m, r, 'corrected', 'Corrected by you' + (always ? '; future mail from ' + m.domain + ' follows this' : ''), {
    label: 'Your correction',
    detail: r ? 'Moved to ' + r.name : 'Back to Inbox',
    meta: always ? 'Saved as a sender rule' : 'Saved as an example',
  });
  return fake(row(m));
}

const undoable = (m: Message) => m.actionId !== null && !m.undone && m.kind !== 'review';

export function undo(actionId: string) {
  const m = messages.find((x) => x.actionId === actionId);
  if (m) m.undone = true;
  return fake(undefined);
}

/** Undo every action on mail received since a unix time, for one rule or for all; resolves to the action ids undone. */
export function undoSince(since: number, ruleId?: string) {
  const hit = messages.filter((m) => undoable(m) && m.receivedAt >= since && (!ruleId || m.ruleId === ruleId));
  for (const m of hit) m.undone = true;
  return fake(hit.map((m) => m.actionId!));
}
