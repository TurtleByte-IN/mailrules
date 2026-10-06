import { describe, expect, it } from 'vitest';
import type { Condition, Rule, RuleInput } from '../../lib/api/rules';
import { condText } from '../rules/text';
import { emptyBuilder, english, fromRule, refusedPart, toCondition, toRow, toRule, type Builder, type Row } from './builder';

describe('condition rows', () => {
  it.each<[Row, unknown, string]>([
    [{ field: 'from_domain', op: 'in', value: 'swiggy.in, zomato.com' }, ['swiggy.in', 'zomato.com'], 'from_domain in swiggy.in, zomato.com'],
    [{ field: 'subject', op: 'contains_any', value: 'invoice' }, ['invoice'], 'subject contains_any invoice'],
    [{ field: 'subject', op: 'matches', value: '^Re: (a|b), c' }, '^Re: (a|b), c', 'subject matches ^Re: (a|b), c'],
    [{ field: 'size_kb', op: 'gt', value: '500' }, 500, 'size_kb gt 500'],
    [{ field: 'has_attachment', op: 'yes', value: '' }, true, 'has_attachment = true'],
    [{ field: 'is_bulk', op: 'no', value: '' }, false, 'is_bulk = false'],
  ])('%j survives the trip to a rule and back', (row, value, text) => {
    const c = toCondition(row);
    expect(c.value).toEqual(value);
    expect(condText(c)).toBe(text);
    expect(toRow(c)).toEqual(row);
  });

  it.each<[Condition, Row]>([
    [{ field: 'header:X-Spam', op: 'exists', value: 'x' }, { field: 'subject', op: 'in', value: 'x' }],
    [{ field: 'subject', op: 'contains', value: 'hi' }, { field: 'subject', op: 'in', value: 'hi' }],
    [{ all: [{ field: 'subject', op: 'in', value: ['x'] }] }, { field: 'subject', op: 'in', value: '' }],
  ])('%j falls back to what the builder offers', (c, row) => {
    expect(toRow(c)).toEqual(row);
  });
});

const full: Builder = {
  ...emptyBuilder(),
  name: 'Invoices',
  match: 'any',
  rows: [
    { field: 'from_domain', op: 'in', value: 'acme.com, bills.io' },
    { field: 'has_attachment', op: 'yes', value: '' },
  ],
  intent: 'An invoice or payment request',
  unless: true,
  folder: 'Finance',
  markRead: true,
  account_id: 2,
  stack: true,
};

describe('builder', () => {
  it('writes the rule the form describes', () => {
    expect(toRule(full)).toMatchObject({
      name: 'Invoices',
      intent: 'An invoice or payment request',
      conditions: {
        any: [
          { field: 'from_domain', op: 'in', value: ['acme.com', 'bills.io'] },
          { field: 'has_attachment', op: 'eq', value: true },
        ],
      },
      exceptions: { all: [{ field: 'replied_before', op: 'eq', value: true }] },
      actions: [{ type: 'move', folder: 'Finance' }, { type: 'read' }],
      account_id: 2,
      stack: true,
    });
  });

  it.each<[string, Partial<Builder>]>([
    ['move and mark read', {}],
    ['archive', { action: 'archive', folder: '', markRead: false }],
    ['trash', { action: 'trash', folder: '', markRead: false }],
    ['keep', { action: 'keep', folder: '' }],
    ['keep and flag', { action: 'flag', folder: '' }],
    ['conditions only', { intent: '', unless: false, match: 'all' }],
  ])('loads a saved rule back into the same form: %s', (_, change) => {
    const b = { ...full, ...change };
    // The daemon's reply to saving this form, shaped as the contract's Rule.
    const saved: Rule = { ...toRule(b), id: 9, said: '', priority: 3, model: '', min_confidence: null, enabled: true, version: 1, created_at: 1791276732, updated_at: 1791276732, hits_week: 0, last_match_at: null };
    expect(fromRule(saved)).toEqual({ ...b, editingId: 9 });
  });

  it('is a RuleInput once it has its wording', () => {
    const input: RuleInput = { ...toRule(full), said: 'Built with conditions', enabled: true };
    expect(Object.keys(input).sort()).toEqual(['account_id', 'actions', 'conditions', 'enabled', 'exceptions', 'intent', 'name', 'said', 'stack']);
  });

  it('leaves out rows with no value and names an unnamed rule', () => {
    const r = toRule({ ...emptyBuilder(), rows: [{ field: 'subject', op: 'in', value: ' ' }], action: 'archive' });
    expect(r).toMatchObject({ name: 'Condition rule', conditions: {}, intent: '', actions: [{ type: 'archive' }] });
  });

  it.each<[Partial<Builder>, string]>([
    [{}, "Emails where the sender domain is acme.com or bills.io or it has an attachment, and that are about an invoice or payment request, unless you've replied to the sender before: move to finance, mark read · stacks · only me@icloud.com."],
    [{ rows: emptyBuilder().rows, unless: false, stack: false, action: 'trash' }, 'Emails about an invoice or payment request: move to trash · only me@icloud.com.'],
    [{ intent: '', unless: false, stack: false, match: 'all', folder: '', markRead: false }, 'Emails where the sender domain is acme.com or bills.io and it has an attachment: move to [folder] · only me@icloud.com.'],
  ])('says it in plain words', (change, want) => {
    expect(english({ ...full, ...change }, 'me@icloud.com')).toBe(want);
  });
});

// Paths as a daemon at contract 0.9 sent them for each refusal.
describe('refusedPart', () => {
  // The empty middle row is not sent, so the daemon's second condition is the form's third row.
  const b: Builder = { ...emptyBuilder(), rows: [{ field: 'from_domain', op: 'in', value: 'a.com' }, { field: 'subject', op: 'in', value: '' }, { field: 'subject', op: 'matches', value: '((' }] };

  it.each<[string, ReturnType<typeof refusedPart>]>([
    ['rules[0].conditions.all[1].op', { part: 'row2', leaf: 'op' }],
    ['rules[0].conditions.any[1].value', { part: 'row2', leaf: 'value' }],
    ['rules[0].conditions.all[0].field', { part: 'row0', leaf: 'field' }],
    ['conditions.all[1].op', { part: 'row2', leaf: 'op' }],
    ['rules[0].conditions.all[5].op', undefined],
    ['rules[0].exceptions.all[0].op', { part: 'unless' }],
    ['rules[0].actions[0].folder', { part: 'folder' }],
    ['rules[0].new_folders[0]', { part: 'folder' }],
    ['rules[0].actions[0].type', { part: 'action' }],
    ['rules[0].actions', { part: 'action' }],
    ['rules[0].min_confidence', { part: 'action' }],
    ['min_confidence', { part: 'action' }],
    ['rules[0].name', { part: 'name' }],
    ['rules[0].stack', { part: 'stack' }],
    ['rules[0].account_id', { part: 'account_id' }],
    ['intent', { part: 'intent' }],
    ['rules[0].model', undefined],
    ['rules[0].conditions', undefined],
    ['rules[0]', undefined],
    ['rules', undefined],
  ])('%s', (path, want) => {
    expect(refusedPart(b, path)).toEqual(want);
  });
});
