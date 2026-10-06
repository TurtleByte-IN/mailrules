// The condition builder's form and its translation to and from a rule.
import { leaves, type Action, type Condition, type Rule, type RulePatch } from '../../lib/api/rules';
import { actionsText, extrasText, fields, treeWords, type Extras, type FieldDef } from '../rules/text';

/** One row of the form. `value` is what the user typed; yes/no fields use `op` alone. */
export interface Row {
  field: string;
  op: string;
  value: string;
}

export interface Builder extends Extras {
  name: string;
  match: 'all' | 'any';
  rows: Row[];
  intent: string;
  /** Except when I've replied to the sender before. */
  unless: boolean;
  action: 'move' | 'archive' | 'trash' | 'keep' | 'flag';
  folder: string;
  markRead: boolean;
  editingId: number | null;
}

export const emptyBuilder = (): Builder => ({
  name: '',
  match: 'all',
  rows: [],
  intent: '',
  unless: false,
  action: 'move',
  folder: '',
  markRead: false,
  editingId: null,
  account_id: null,
  stack: false,
});

export function opsFor(type: FieldDef['type']): [id: string, name: string][] {
  if (type === 'bool') return [['yes', 'is yes'], ['no', 'is no']];
  if (type === 'num') return [['gt', 'is more than'], ['lt', 'is less than']];
  return [['in', 'is any of'], ['contains_any', 'contains any of'], ['matches', 'matches pattern']];
}

export function toCondition(r: Row): Condition {
  const type = fields[r.field].type;
  const op = r.op as Condition['op'];
  if (type === 'bool') return { field: r.field, op: 'eq', value: r.op === 'yes' };
  if (type === 'num') return { field: r.field, op, value: Number(r.value) };
  if (r.op === 'matches') return { field: r.field, op, value: r.value.trim() };
  return { field: r.field, op, value: r.value.split(',').map((v) => v.trim()).filter(Boolean) };
}

/** A field or operator the builder does not offer falls back to the first one it does. */
export function toRow(c: Condition): Row {
  const field = c.field && fields[c.field] ? c.field : 'subject';
  if (typeof c.value === 'boolean') return { field, op: c.value ? 'yes' : 'no', value: '' };
  const ops = opsFor(fields[field].type).map((o) => o[0]);
  return { field, op: c.op && ops.includes(c.op) ? c.op : ops[0], value: [c.value].flat().join(', ') };
}

/** Rows that say something: yes/no rows always do, the others need a value. */
export const filled = (b: Builder) => b.rows.filter((r) => fields[r.field].type === 'bool' || r.value.trim());

/**
 * The rule this form describes, without the fields the builder does not own (wording, model,
 * threshold). It is a patch as it stands; with `said` and `enabled` it is a RuleInput.
 */
export function toRule(b: Builder): Required<Pick<RulePatch, 'name' | 'intent' | 'conditions' | 'exceptions' | 'actions' | 'account_id' | 'stack'>> {
  const rows = filled(b);
  const folder = b.folder.trim();
  const first: Action[] = b.action === 'flag' ? [{ type: 'keep' }, { type: 'flag' }] : b.action === 'move' ? [{ type: 'move', folder }] : [{ type: b.action }];
  return {
    name: b.name.trim() || (b.action === 'move' ? folder : 'Condition rule'),
    intent: b.intent.trim(),
    conditions: rows.length ? { [b.match]: rows.map(toCondition) } : {},
    exceptions: b.unless ? { all: [{ field: 'replied_before', op: 'eq', value: true }] } : {},
    actions: b.markRead && b.action !== 'trash' ? [...first, { type: 'read' }] : first,
    account_id: b.account_id,
    stack: b.stack,
  };
}

/**
 * Where on the form a refused save belongs. `path` is as the daemon sends it:
 * "rules[0].conditions.all[1].op" from a batch save, "conditions.all[1].op" from an edit.
 * `part` is "row<i>" (an index into `b.rows`; the daemon counts only the filled rows) or a
 * form field, and `leaf` the control within a row. Nothing when the form has no control for it.
 */
export function refusedPart(b: Builder, path: string): { part: string; leaf?: string } | undefined {
  const p = path.replace(/^rules\[\d+\]\./, '');
  const row = /^conditions\.(?:all|any)\[(\d+)\](?:\.(field|op|value))?/.exec(p);
  if (row) {
    const i = b.rows.indexOf(filled(b)[+row[1]]);
    return i < 0 ? undefined : { part: 'row' + i, leaf: row[2] ?? 'value' };
  }
  if (p.startsWith('exceptions')) return { part: 'unless' };
  if (/^actions\[\d+\]\.folder|^new_folders/.test(p)) return { part: 'folder' };
  // The form has no threshold; the one it can break is the higher one a rule that trashes needs.
  if (p.startsWith('actions') || p === 'min_confidence') return { part: 'action' };
  if (['name', 'intent', 'account_id', 'stack'].includes(p)) return { part: p };
}

// ponytail: an exception richer than "replied before" collapses to that checkbox; add an
// exceptions editor when the composer starts producing others that people want to edit.
export function fromRule(r: Rule): Builder {
  const has = (type: Action['type']) => r.actions.some((a) => a.type === type);
  const rows = leaves(r.conditions).map(toRow);
  return {
    name: r.name,
    match: r.conditions.any ? 'any' : 'all',
    rows,
    intent: r.intent,
    unless: leaves(r.exceptions).length > 0,
    action: has('trash') ? 'trash' : has('move') ? 'move' : has('archive') ? 'archive' : has('flag') ? 'flag' : 'keep',
    folder: r.actions.find((a) => a.type === 'move')?.folder ?? '',
    markRead: has('read'),
    editingId: r.id,
    account_id: r.account_id,
    stack: r.stack,
  };
}

/** The form as one sentence. `only` is the mailbox address when the rule applies to one. */
export function english(b: Builder, only?: string) {
  const r = toRule(b);
  const where = treeWords(r.conditions);
  let s = where ? 'Emails where ' + where : 'Emails';
  if (r.intent) s += (where ? ', and that are about ' : ' about ') + r.intent.replace(/^an? /i, (m) => m.toLowerCase());
  if (b.unless) s += ", unless you've replied to the sender before";
  const does = actionsText(r.actions);
  return s + ': ' + does[0].toLowerCase() + does.slice(1) + extrasText(b, only) + '.';
}
