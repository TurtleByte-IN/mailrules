// DEMO: GET /api/rules, PATCH DELETE /api/rules/{id}, POST /api/rules/reorder,
// POST /api/rules/batch and POST /api/rules/test are not in api/openapi.yaml yet (backend M7, M8).
// Shapes follow docs/backend-plan.md → Data model and Rules and condition matcher;
// `hits`, `trash` and the P2 fields come from the prototype.
import { fake, fakeId } from './demo';

export interface Condition {
  field: string;
  op: string;
  value: string | string[] | number | boolean;
}

/** One group of conditions; empty means "always true". Nested groups are not modelled yet. */
export interface ConditionTree {
  all?: Condition[];
  any?: Condition[];
}

export interface Action {
  type: 'move' | 'archive' | 'trash' | 'junk' | 'flag' | 'unflag' | 'read' | 'unread' | 'keep';
  folder?: string;
}

/** The "more options" of a rule. `later`, `notify` and the draft fields are P2 (lib/features.ts). */
export interface Extras {
  /** null = all mailboxes. */
  account_id: string | null;
  stack: boolean;
  later?: { on: boolean; after: string; action: string };
  notify?: string;
  draft?: boolean;
  draftNote?: string;
}

/** What the client sends to create a rule. */
export interface RuleInput extends Extras {
  name: string;
  /** The user's original wording. */
  said: string;
  /** null = condition-only. */
  intent: string | null;
  conditions: ConditionTree;
  /** The "unless" of a rule. */
  exceptions: ConditionTree;
  actions: Action[];
  /** Per-rule decider override; null = the default decider. */
  model: string | null;
  min_confidence: number | null;
}

export interface Rule extends RuleInput {
  id: string;
  enabled: boolean;
  /** True when the rule's action moves mail to Trash. */
  trash: boolean;
  /** Emails this rule acted on this week. */
  hits: number;
}

export type RulePatch = Partial<RuleInput & { enabled: boolean }>;

/** Summary of a tester run. The daemon will return one row per email; this is what the screens show. */
export interface TestSummary {
  limit: number;
  matched: number;
  model_calls: number;
  /** null when no email reached a model. */
  avg_confidence: number | null;
  to_review: number;
  latest_from: string | null;
}

export const leaves = (t: ConditionTree) => t.all ?? t.any ?? [];
export const isTrash = (actions: Action[]) => actions.some((a) => a.type === 'trash');

const seed = (r: Pick<Rule, 'id' | 'name' | 'said' | 'actions' | 'hits'> & Partial<Rule>): Rule => ({
  account_id: null,
  intent: null,
  conditions: {},
  exceptions: {},
  stack: false,
  model: null,
  min_confidence: 0.75,
  enabled: true,
  ...r,
  trash: isTrash(r.actions),
});

let rules: Rule[] = [
  seed({
    id: 'r1',
    name: 'Food orders',
    said: 'Put all Swiggy and Zomato stuff in Food',
    conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in', 'zomato.com'] }] },
    actions: [{ type: 'move', folder: 'Food' }, { type: 'read' }],
    hits: 38,
  }),
  seed({
    id: 'r2',
    name: 'Login codes',
    said: 'Keep OTPs and login codes, and flag them',
    conditions: { all: [{ field: 'subject', op: 'contains_any', value: ['code', 'OTP', 'verification'] }] },
    actions: [{ type: 'keep' }, { type: 'flag' }],
    hits: 12,
  }),
  seed({
    id: 'r3',
    name: 'Recruiters',
    said: "Recruiter emails go to Jobs unless I've talked to them before",
    intent: 'Recruiter outreach about job openings',
    exceptions: { all: [{ field: 'replied_before', op: 'eq', value: true }] },
    actions: [{ type: 'move', folder: 'Jobs' }],
    hits: 9,
  }),
  seed({
    id: 'r4',
    name: 'Scams',
    said: 'Trash anything that looks like a fake bank alert',
    intent: 'Phishing, scams or fake bank alerts',
    actions: [{ type: 'trash' }],
    hits: 3,
    min_confidence: 0.9,
  }),
  seed({
    id: 'r5',
    name: 'Newsletters',
    said: 'Newsletters I never read go to Reading',
    intent: 'Newsletters and promotional emails',
    actions: [{ type: 'move', folder: 'Reading' }, { type: 'read' }],
    hits: 51,
  }),
  seed({
    id: 'r6',
    name: 'Orders',
    said: 'Delivery and shipping updates go to Shopping',
    intent: 'Order confirmations, delivery and shipping updates',
    actions: [{ type: 'move', folder: 'Shopping' }],
    hits: 17,
    stack: true,
  }),
];

// Callers hand in reactive state, which structuredClone refuses; a real request body is JSON anyway.
const plain = <T>(v: T): T => JSON.parse(JSON.stringify(v));

export const list = () => fake(rules);

export function patch(id: string, p: RulePatch) {
  rules = rules.map((r) => (r.id === id ? seed({ ...r, ...plain(p) }) : r));
  return fake(rules.find((r) => r.id === id)!);
}

export function remove(id: string) {
  rules = rules.filter((r) => r.id !== id);
  return fake(null);
}

/** Sets the order rules are checked in; returns the list in that order. */
export function reorder(ids: string[]) {
  rules = ids.flatMap((id) => rules.find((r) => r.id === id) ?? []);
  return fake(rules);
}

/** Saves new rules at the bottom of the list. `new_folders` are created by the daemon. */
export function batch(inputs: (RuleInput & { new_folders?: string[] })[]) {
  const added = plain(inputs).map(({ new_folders: _, ...r }) => seed({ ...r, id: fakeId('r'), hits: 0 }));
  rules = [...rules, ...added];
  return fake(added);
}

/** Runs a saved or draft rule over recent mail without acting. */
export function test(rule: Pick<RuleInput, 'intent' | 'conditions'>, limit = 200): Promise<TestSummary> {
  const conds = leaves(rule.conditions);
  const matched = ((conds.map((c) => String(c.value)).join('').length + conds.length * 3) * 7) % 17 + 3;
  const first = conds[0];
  return fake({
    limit,
    matched,
    model_calls: rule.intent ? Math.max(1, Math.round(matched / 4)) : 0,
    avg_confidence: rule.intent ? 0.89 : null,
    to_review: rule.intent ? 1 : 0,
    latest_from: first?.field === 'from_domain' ? String([first.value].flat()[0]) : null,
  });
}
