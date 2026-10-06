import { beforeEach, expect, it, vi } from 'vitest';

// The demo API keeps its data in the module, so every test starts from a fresh copy.
let m: typeof import('./senders.svelte');
let toast: { text: string };

beforeEach(async () => {
  vi.resetModules();
  m = await import('./senders.svelte');
  toast = (await import('./toast.svelte')).toast;
  await (await import('./rules.svelte')).load();
  await m.load();
});

const sender = (name: string) => m.senders.list.find((s) => s.name === name)!;

it.each([
  ['', 8],
  ['swiggy', 1],
  ['  LINKEDIN ', 1],
  ['.in', 2],
  ['mehta.studio', 1],
  ['nobody', 0],
])('search %j finds %i senders', (query, n) => expect(m.search(m.senders.list, query)).toHaveLength(n));

it.each([
  ['volume', 'LinkedIn'],
  ['unread', 'Myntra'],
] as const)('sort %s puts %s first', async (sort, first) => {
  await m.setSort(sort);
  expect(m.senders.list[0].name).toBe(first);
});

it('seed rules show as routing', () => {
  expect(m.routingOf(sender('Weekly Go Digest'))).toBe('r5');
  expect(m.routingOf(sender('HDFC Bank'))).toBe('keep');
  expect(m.routingOf(sender('Swiggy'))).toBe('auto');
});

it.each([
  ['keep', 'Mail from Swiggy: Keep in Inbox'],
  ['trash', 'Mail from Swiggy: Trash'],
  ['r1', 'Mail from Swiggy: Food orders'],
])('routing %s', async (routing, text) => {
  await m.setRouting(sender('Swiggy'), routing);
  expect(m.routingOf(sender('Swiggy'))).toBe(routing);
  expect(m.senders.rules[0]).toMatchObject({ type: 'domain', value: 'swiggy.in', source: 'user' });
  expect(toast.text).toBe(text);
});

it('routing auto removes the sender rule', async () => {
  await m.setRouting(sender('HDFC Bank'), 'auto');
  expect(m.routingOf(sender('HDFC Bank'))).toBe('auto');
  expect(m.senders.rules.some((r) => r.value === 'hdfcbank.net')).toBe(false);
  expect(toast.text).toBe('HDFC Bank goes back to your rules');
});

it('unsubscribe trashes stragglers but keeps a routing the user already set', async () => {
  await m.unsubscribe(sender('Myntra'));
  expect(sender('Myntra').unsubscribed).toBe(true);
  expect(m.routingOf(sender('Myntra'))).toBe('trash');
  expect(toast.text).toBe('Unsubscribed from Myntra via List-Unsubscribe. Stragglers go to Trash.');

  await m.unsubscribe(sender('Weekly Go Digest'));
  expect(m.routingOf(sender('Weekly Go Digest'))).toBe('r5');
});

it('forget removes a learned rule', async () => {
  const learned = m.senders.rules.find((r) => r.value === 'jobalerts.in')!;
  expect(m.targetOf(learned)).toBe('Recruiters');
  await m.forget(learned);
  expect(m.senders.rules.some((r) => r.value === 'jobalerts.in')).toBe(false);
});
