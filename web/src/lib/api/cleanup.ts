// DEMO: POST /api/cleanup/preview, POST /api/cleanup/run, GET /api/batches/{id} and
// POST /api/batches/{id}/undo are not in api/openapi.yaml yet (backend M9). GET /api/batches
// (the list) is in no plan yet; see list() below.
import { fake, fakeId } from './demo';

export interface Scope {
  accountId: string;
  folder: 'INBOX' | 'Archive' | 'all';
  /** Days back, or all time. */
  range: '30' | '90' | '365' | 'all';
}

export interface PreviewRow {
  /** "Newsletters → Reading", or what happens to mail no rule moves. */
  name: string;
  kind: 'rule' | 'trash' | 'inbox' | 'review';
  count: number;
  samples: { sender: string; subject: string }[];
}

export interface Preview {
  total: number;
  rows: PreviewRow[];
}

/** A batches row from docs/backend-plan.md → Data model, plus what batch.progress reports. */
export interface Batch {
  id: string;
  kind: 'cleanup';
  status: 'running' | 'done' | 'failed' | 'undone';
  total: number;
  done: number;
  createdAt: number;
  scope: Scope;
  tokens: number;
  costUsd: number;
}

const now = () => Math.floor(Date.now() / 1000);

const rows: PreviewRow[] = [
  { name: 'Newsletters → Reading', kind: 'rule', count: 168, samples: [{ sender: 'Weekly Go Digest', subject: 'Issue 212: generics in practice' }] },
  { name: 'Orders → Shopping', kind: 'rule', count: 61, samples: [{ sender: 'Amazon.in', subject: 'Your package was delivered' }] },
  { name: 'Food orders → Food', kind: 'rule', count: 44, samples: [{ sender: 'Swiggy', subject: 'Your order from Meghana Foods is on the way' }] },
  { name: 'Recruiters → Jobs', kind: 'rule', count: 19, samples: [{ sender: 'Priya, TalentBridge', subject: 'Senior backend role, Go and Kubernetes, remote' }] },
  { name: 'Scams → Trash', kind: 'trash', count: 6, samples: [{ sender: 'HDFC Bank Alerts', subject: 'Your account will be blocked today, verify KYC' }] },
  { name: 'Left in Inbox', kind: 'inbox', count: 102, samples: [{ sender: 'Rahul Mehta', subject: 'Re: invoice for September' }] },
  { name: 'Needs review', kind: 'review', count: 12, samples: [{ sender: 'JobAlerts', subject: '12 new jobs matching Backend Developer' }] },
];

let batches: Batch[] = [
  { id: 'k1', kind: 'cleanup', status: 'done', total: 412, done: 412, createdAt: now() - 86400, scope: { accountId: 'acc1', folder: 'INBOX', range: '30' }, tokens: 36080, costUsd: 0.01 },
];

function count(scope: Scope): Preview {
  const m = { '30': 1, '90': 3, '365': 11, all: 26 }[scope.range] * { INBOX: 1, Archive: 0.7, all: 1.6 }[scope.folder];
  const scaled = rows.map((r) => ({ ...r, count: Math.round(r.count * m) }));
  return { total: scaled.reduce((a, r) => a + r.count, 0), rows: scaled };
}

const save = (b: Batch) => {
  batches = batches.map((x) => (x.id === b.id ? b : x));
  return fake(b);
};

export const preview = (scope: Scope) => fake(count(scope));

export function run(scope: Scope) {
  const batch: Batch = { id: fakeId('k'), kind: 'cleanup', status: 'running', total: count(scope).total, done: 0, createdAt: now(), scope, tokens: 0, costUsd: 0 };
  batches = [batch, ...batches];
  return fake(batch);
}

/** The demo has no worker, so a running batch moves 7% further each time it is read. */
export function get(id: string) {
  const b = batches.find((x) => x.id === id)!;
  if (b.status !== 'running') return fake(b);
  const done = Math.min(b.total, b.done + Math.ceil(b.total * 0.07));
  // About a fifth of mail reaches a model; the rest is decided by conditions and sender rules.
  return save({ ...b, done, status: done === b.total ? 'done' : 'running', tokens: done * 88, costUsd: done * 0.22 * 0.00011 });
}

export const undo = (id: string) => save({ ...batches.find((x) => x.id === id)!, status: 'undone' });

/** Batches that can still be undone, newest first. */
export const list = () => fake(batches.filter((b) => b.status !== 'running'));
