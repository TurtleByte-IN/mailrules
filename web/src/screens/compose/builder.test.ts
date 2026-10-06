import { describe, expect, it } from 'vitest';
import type { Rule } from '../../lib/api/rules';
import { condText } from '../rules/text';
import { emptyBuilder, english, fromRule, toCondition, toRow, toRule, type Builder, type Row } from './builder';

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

  it.each([
    [{ field: 'header:X-Spam', op: 'exists', value: 'x' }, { field: 'subject', op: 'in', value: 'x' }],
    [{ field: 'subject', op: 'contains', value: 'hi' }, { field: 'subject', op: 'in', value: 'hi' }],
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
  account_id: 'acc1',
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
      account_id: 'acc1',
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
    const saved = { ...toRule(b), id: 'r9', said: '', model: null, min_confidence: 0.75, enabled: true, trash: false, hits: 0 } satisfies Rule;
    expect(fromRule(saved)).toEqual({ ...b, editingId: 'r9' });
  });

  it('leaves out rows with no value and names an unnamed rule', () => {
    const r = toRule({ ...emptyBuilder(), rows: [{ field: 'subject', op: 'in', value: ' ' }], action: 'archive' });
    expect(r).toMatchObject({ name: 'Condition rule', conditions: {}, intent: null, actions: [{ type: 'archive' }] });
  });

  it.each<[Partial<Builder>, string]>([
    [{}, "Emails where the sender domain is acme.com or bills.io or it has an attachment, and that are about an invoice or payment request, unless you've replied to the sender before: move to finance, mark read · stacks · only me@icloud.com."],
    [{ rows: emptyBuilder().rows, unless: false, stack: false, action: 'trash' }, 'Emails about an invoice or payment request: move to trash · only me@icloud.com.'],
    [{ intent: '', unless: false, stack: false, match: 'all', folder: '', markRead: false }, 'Emails where the sender domain is acme.com or bills.io and it has an attachment: move to [folder] · only me@icloud.com.'],
  ])('says it in plain words', (change, want) => {
    expect(english({ ...full, ...change }, 'me@icloud.com')).toBe(want);
  });
});
