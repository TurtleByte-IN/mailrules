import { cleanup, fireEvent, render, screen } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { Rule } from '../lib/api/rules';
import { load, rules } from '../lib/state/rules.svelte';
import { settings } from '../lib/state/settings.svelte';
import Rules from './Rules.svelte';

// A rule as GET /api/rules returns it.
const rule = (id: number, name: string): Rule => ({
  id,
  account_id: null,
  name,
  said: '',
  intent: '',
  conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
  exceptions: {},
  actions: [{ type: 'move', folder: name }],
  priority: id,
  stack: false,
  model: '',
  min_confidence: null,
  enabled: true,
  version: 1,
  created_at: 1791276732,
  updated_at: 1791276732,
  hits_week: 3,
  last_match_at: null,
});

/** Answers every request with [status, body]. */
function respond(status: number, body: unknown) {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => new Response(JSON.stringify(body), { status }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

beforeEach(() => Object.assign(rules, { list: [], loaded: false, error: '' }));
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

it('shows a failed load as an alert, and Retry fetches the rules again', async () => {
  respond(500, { error: { code: 'internal', message: 'Something went wrong on the server.' } });
  await load();
  render(Rules);
  expect(screen.getByRole('alert').textContent).toContain('Your rules could not be loaded. Something went wrong on the server.');
  expect(screen.queryByText('No rules yet.')).toBeNull();

  const f = respond(200, { items: [rule(1, 'Food')] });
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(f.mock.calls[0][0]).toBe('/api/rules');
  expect(await screen.findByRole('button', { name: /^Food/ })).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('says so when there are no rules', async () => {
  respond(200, { items: [] });
  await load();
  render(Rules);
  expect(screen.getByText('No rules yet.')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it.each([
  [true, 'Then: Move to MailRules Trash'],
  [false, 'Then: Move to Trash'],
])('with trash_to_folder %s, a trash rule names where its mail goes: %s', async (on, text) => {
  settings.value.trash_to_folder = on;
  try {
    respond(200, { items: [{ ...rule(1, 'Scams'), actions: [{ type: 'trash' }] }] });
    await load();
    render(Rules);
    expect(screen.getByRole('region', { name: 'Rule list' }).querySelector('li')!.textContent).toContain(text);
  } finally {
    settings.value.trash_to_folder = true;
  }
});

it('lists the rules in order, and asks for a mailbox before testing one', async () => {
  const f = respond(200, { items: [rule(1, 'Food'), rule(2, 'Travel')] });
  await load();
  render(Rules);
  const rows = screen.getByRole('region', { name: 'Rule list' }).querySelectorAll('li');
  expect([...rows].map((li) => li.querySelector('.font-semibold')?.textContent)).toEqual(['Food', 'Travel']);
  expect(rows[0].textContent).toContain('from_domain in swiggy.in');
  expect(rows[0].textContent).toContain('Then: Move to Food · 3 this week');

  // No mailbox is connected: the tester sends nothing and says what to do.
  await fireEvent.click(screen.getByRole('button', { name: 'Test on last 200 emails' }));
  expect(screen.getByRole('link', { name: 'Connect a mailbox' }).getAttribute('href')).toBe('#/accounts');
  expect(f).toHaveBeenCalledOnce();
});
