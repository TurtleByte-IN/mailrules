// DEMO: POST /api/rules/compose is not in api/openapi.yaml yet (backend M8).
// The draft shape follows docs/backend-plan.md → Rule composer and tester; `options` and
// `answer` (how a question is answered) come from the prototype.
import { fake } from './demo';
import type { RuleInput } from './rules';

/** A rule the composer proposes. Nothing is saved until the user approves it. */
export interface Draft extends RuleInput {
  /** Folders the rule names that do not exist yet. */
  new_folders: string[];
  /** At most one per draft. */
  question: string | null;
  options: string[];
  answer: string;
  conflicts: { rule_id: string; kind: string; note: string }[];
  /** Out of the last 200 emails. */
  match_count: number;
  samples: string[];
}

export const sample =
  "Bank statements and credit card bills go to Finance and mark them read. Archive LinkedIn emails about who viewed my profile. Cold sales pitches from people I've never emailed go to Trash.";

const draft = (d: Pick<Draft, 'name' | 'said' | 'actions' | 'match_count'> & Partial<Draft>): Draft => ({
  account_id: null,
  stack: false,
  intent: null,
  conditions: {},
  exceptions: {},
  model: null,
  min_confidence: null,
  new_folders: [],
  question: null,
  options: [],
  answer: '',
  conflicts: [],
  samples: [],
  ...d,
});

const curated: Draft[] = [
  draft({
    name: 'Finance statements',
    said: 'Bank statements and credit card bills go to Finance and mark them read',
    intent: 'Bank statements, credit card bills and payment reminders',
    actions: [{ type: 'move', folder: 'Finance' }, { type: 'read' }],
    new_folders: ['Finance'],
    match_count: 11,
    samples: ['HDFC credit card statement for September', 'ICICI e-statement', 'Airtel bill due 12 Oct'],
  }),
  draft({
    name: 'LinkedIn profile views',
    said: 'Archive LinkedIn emails about who viewed my profile',
    conditions: {
      all: [
        { field: 'from_domain', op: 'in', value: ['linkedin.com'] },
        { field: 'subject', op: 'contains_any', value: ['viewed your profile'] },
      ],
    },
    actions: [{ type: 'archive' }],
    match_count: 6,
    samples: ['Ananya and 3 others viewed your profile', 'You appeared in 9 searches'],
  }),
  draft({
    name: 'Cold sales',
    said: "Cold sales pitches from people I've never emailed go to Trash",
    intent: 'Cold sales pitches and unsolicited demo requests',
    exceptions: {
      any: [
        { field: 'is_contact', op: 'eq', value: true },
        { field: 'replied_before', op: 'eq', value: true },
      ],
    },
    actions: [{ type: 'trash' }],
    match_count: 4,
    samples: ['15 minutes to cut your AWS bill?', 'Quick question about your hiring'],
    conflicts: [{ rule_id: 'r5', kind: 'overlap', note: 'Overlaps with Newsletters: some vendor newsletters read like pitches. Cold sales sits above it, so it wins.' }],
    question: 'Trash them, or keep them in a Sales folder?',
    options: ['Trash', 'Move to Sales'],
    answer: 'Trash',
  }),
];

// ponytail: keyword matching stands in for the composer model; it goes when the endpoint lands.
function naive(text: string): Draft[] {
  return text
    .split(/\n+|[.;!?]+(?:\s+|$)|,\s*(?:and\s+)?/)
    .map((p) => p.trim())
    .filter((p) => p.length > 6)
    .slice(0, 6)
    .map((p, i) => {
      const dom = p.match(/\b[a-z0-9-]+\.(?:com|in|co|org|net|io|dev)\b/i)?.[0];
      const folder = p.match(/\b(?:to|in|into)\s+(?:the\s+|a\s+|my\s+)?([A-Z][A-Za-z]+)/)?.[1] ?? '';
      const trash = /\b(trash|delete|bin)\b/i.test(p);
      const archive = /\barchive\b/i.test(p);
      const ask = !folder && !trash && !archive;
      const first: Draft['actions'][number] = trash ? { type: 'trash' } : archive ? { type: 'archive' } : folder ? { type: 'move', folder } : { type: 'keep' };
      return draft({
        name: folder || (trash ? 'Trash ' : 'Rule ') + (i + 1),
        said: p,
        intent: (p[0].toUpperCase() + p.slice(1)).replace(/\s+(?:go(?:es)?|should go|move|moved)\s+(?:to|in|into)\s+.*$/i, ''),
        conditions: dom ? { all: [{ field: 'from_domain', op: 'in', value: [dom.toLowerCase()] }] } : {},
        actions: /mark (?:it |them )?(?:as )?read/i.test(p) ? [first, { type: 'read' }] : [first],
        new_folders: first.type === 'move' && !['Food', 'Jobs', 'Reading', 'Shopping'].includes(folder) ? [folder] : [],
        match_count: ((p.length * 7) % 13) + 2,
        question: ask ? 'Where should these emails go?' : null,
        options: ask ? ['Keep in Inbox', 'Move to Later'] : [],
        answer: ask ? 'Keep in Inbox' : '',
      });
    });
}

/** Turns typed or dictated text (up to 4,000 characters) into draft rules. */
export const compose = (text: string) => fake(text === sample ? curated : naive(text));
