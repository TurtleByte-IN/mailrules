import { fireEvent, render, screen, waitFor } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { Account } from '../lib/api/accounts';
import type { Sender } from '../lib/api/senders';
import { day } from '../lib/format';
import { accounts } from '../lib/state/accounts.svelte';
import { rules } from '../lib/state/rules.svelte';
import { senders } from '../lib/state/senders.svelte';
import { settings } from '../lib/state/settings.svelte';
import { toast } from '../lib/state/toast.svelte';
import Senders from './Senders.svelte';

type Reply = [status: number, body: unknown];
let routes: Record<string, Reply>;

const sender = (value: string, over: Partial<Sender> = {}): Sender => ({
  type: 'address',
  value,
  name: '',
  messages: 0,
  last_seen_at: null,
  has_list_unsubscribe: false,
  one_click_unsubscribe: false,
  unsubscribed_at: null,
  verdict: null,
  rule_id: null,
  folder: null,
  source: null,
  hits: 0,
  ...over,
});
const swiggy = sender('no-reply@swiggy.in', { name: 'Swiggy', messages: 64, last_seen_at: 1790000000 });
const quiet = sender('made.up@example.test', { verdict: 'keep', source: 'user' });
const jobs = sender('jobalerts.in', { type: 'domain', messages: 12, verdict: 'route', rule_id: 5, source: 'learned', hits: 3 });

const LIST = 'GET /api/senders?sort=volume';
const LEARNED = 'GET /api/senders?source=learned&limit=100';
const page = (items: Sender[]): Reply => [200, { items, next_cursor: null }];

beforeEach(() => {
  // The state lives in module scope; each test starts it over.
  Object.assign(senders, { status: 'loading', error: '', list: [], next: null, learned: [], sort: 'volume', q: '', folders: [] });
  Object.assign(rules, { list: [{ id: 5, name: 'Recruiters' }] });
  accounts.list = [];
  routes = { [LIST]: page([swiggy, jobs, quiet]), [LEARNED]: page([jobs]) };
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init: RequestInit) => {
      const [status, body] = routes[init.method + ' ' + url] ?? [404, { error: { code: 'not_found', message: 'no route ' + url } }];
      return new Response(JSON.stringify(body), { status });
    }),
  );
});
afterEach(() => vi.unstubAllGlobals());

it('a sender with one email says "1 email", not "1 emails"', async () => {
  routes[LIST] = page([sender('once@example.test', { messages: 1 })]);
  render(Senders);
  expect(await screen.findByText('1 email')).toBeTruthy();
  expect(screen.queryByText('1 emails')).toBeNull();
});

it('a failed load shows an alert, and Retry fetches again', async () => {
  routes[LIST] = [500, { error: { code: 'internal', message: 'Something went wrong.' } }];
  render(Senders);
  expect((await screen.findByRole('alert')).textContent).toContain('Something went wrong.');

  routes[LIST] = page([swiggy]);
  await fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
  expect(await screen.findByText('Swiggy')).toBeTruthy();
  expect(screen.queryByRole('alert')).toBeNull();
});

it('no senders and nothing learned say so', async () => {
  routes[LIST] = page([]);
  routes[LEARNED] = page([]);
  render(Senders);
  expect(await screen.findByText('No senders found.')).toBeTruthy();
  expect(screen.getByText('Nothing learned yet.')).toBeTruthy();
});

it('shows names, addresses, domain rows, routing and the learned rules', async () => {
  render(Senders);
  expect(await screen.findByText('Swiggy')).toBeTruthy();
  expect(screen.getByText('no-reply@swiggy.in')).toBeTruthy();
  expect(screen.getByText('64 emails')).toBeTruthy();

  // A domain has no display name: it goes by its value and says what it covers.
  expect(screen.getByText('Domain: all its addresses')).toBeTruthy();
  expect((screen.getByLabelText('What happens to mail from jobalerts.in') as HTMLSelectElement).value).toBe('5');
  expect((screen.getByLabelText('What happens to mail from made.up@example.test') as HTMLSelectElement).value).toBe('keep');
  expect((screen.getByLabelText('What happens to mail from Swiggy') as HTMLSelectElement).value).toBe('auto');

  const learned = screen.getByRole('region', { name: 'Learned sender rules' });
  expect(learned.textContent).toContain('jobalerts.in → Recruiters');
  expect(screen.getByRole('button', { name: 'Forget jobalerts.in' })).toBeTruthy();
});

it('says when each sender last wrote, and nothing for one with no mail in 30 days', async () => {
  render(Senders);
  expect(await screen.findByText('last wrote ' + day(swiggy.last_seen_at!))).toBeTruthy();
  expect(screen.getAllByText(/^last wrote /)).toHaveLength(1);
});

const folder = (name: string) => ({ name, delimiter: '/', special_use: '' as const });

it("offers every mailbox's folders, Inbox left out, and files a sender in the one chosen", async () => {
  accounts.list = [{ id: 1 }, { id: 2 }] as Account[];
  routes['GET /api/accounts/1/folders'] = [200, { items: [folder('INBOX'), folder('Receipts'), folder('Archive')] }];
  routes['GET /api/accounts/2/folders'] = [200, { items: [folder('INBOX'), folder('Receipts'), folder('Work/Clients')] }];
  const saved = { ...swiggy, verdict: 'move', folder: 'Receipts', source: 'user' };
  routes['PUT /api/senders/address/no-reply%40swiggy.in'] = [200, { sender: saved }];
  render(Senders);

  const select = (await screen.findByLabelText('What happens to mail from Swiggy')) as HTMLSelectElement;
  await waitFor(() => expect(select.textContent).toContain('Always move to Receipts'));
  expect([...select.options].map((o) => o.text)).toEqual([
    'Let my rules decide',
    'Always keep in Inbox',
    'Always: Recruiters',
    'Always move to Archive',
    'Always move to Receipts',
    'Always move to Work/Clients',
    'Always trash',
  ]);

  await fireEvent.change(select, { target: { value: 'move:Receipts' } });
  await waitFor(() => expect(toast.text).toBe('Mail from Swiggy: Move to Receipts'));
  const put = vi.mocked(fetch).mock.calls.find(([, init]) => init?.method === 'PUT')!;
  expect(JSON.parse(put[1]!.body as string)).toEqual({ verdict: 'move', folder: 'Receipts' });
  expect(select.value).toBe('move:Receipts');
});

it('keeps a sender\'s folder on offer when no mailbox lists it', async () => {
  routes[LIST] = page([sender('a@b.example', { name: 'Gone', verdict: 'move', folder: 'Old stuff', source: 'user' })]);
  render(Senders);
  const select = (await screen.findByLabelText('What happens to mail from Gone')) as HTMLSelectElement;
  expect(select.value).toBe('move:Old stuff');
  expect(select.selectedOptions[0].text).toBe('Always move to Old stuff');
});

describe('one-click unsubscribe', () => {
  const news = sender('news@one.example', { name: 'One Weekly', messages: 30, has_list_unsubscribe: true, one_click_unsubscribe: true });
  const mailto = sender('news@two.example', { name: 'Two Digest', messages: 9, has_list_unsubscribe: true });
  const UNSUB = 'POST /api/senders/address/news%40one.example/unsubscribe';
  const button = () => screen.getByRole('button', { name: /^Unsub/ }) as HTMLButtonElement;

  beforeEach(() => {
    settings.value.features.unsubscribe = true;
    routes[LIST] = page([news, mailto, swiggy, jobs]);
  });
  afterEach(() => {
    settings.value.features.unsubscribe = false;
  });

  it('offers Unsubscribe only for a sender with a one-click link, and nothing for a domain', async () => {
    render(Senders);
    await screen.findByText('One Weekly');
    expect(screen.getAllByRole('button', { name: 'Unsubscribe' })).toHaveLength(1);
    // A mailto: link only, or no List-Unsubscribe at all: no button; Always trash is in the menu.
    expect(screen.getAllByText('No unsubscribe link')).toHaveLength(2);
  });

  it('shows nothing while the daemon says the feature is off', async () => {
    settings.value.features.unsubscribe = false;
    render(Senders);
    await screen.findByText('One Weekly');
    expect(screen.queryByRole('button', { name: 'Unsubscribe' })).toBeNull();
    expect(screen.queryByText('No unsubscribe link')).toBeNull();
  });

  it('one click unsubscribes: the row says so and sends the stragglers to Trash', async () => {
    const done = { ...news, unsubscribed_at: 1791000000, verdict: 'block', source: 'user' };
    routes[UNSUB] = [200, { sender: done }];
    render(Senders);
    await screen.findByText('One Weekly');
    await fireEvent.click(button());
    await waitFor(() => expect(button().textContent?.trim()).toBe('Unsubscribed'));
    expect(button().disabled).toBe(true);
    expect(toast.text).toBe('Unsubscribed from One Weekly via List-Unsubscribe. Stragglers go to Trash.');
    expect((screen.getByLabelText('What happens to mail from One Weekly') as HTMLSelectElement).value).toBe('trash');
    expect(vi.mocked(fetch).mock.calls.filter(([, init]) => init?.method === 'POST')).toHaveLength(1);
  });

  it('a sender unsubscribed before shows it is done', async () => {
    routes[LIST] = page([{ ...news, unsubscribed_at: 1791000000 }]);
    render(Senders);
    await screen.findByText('One Weekly');
    expect(button().textContent?.trim()).toBe('Unsubscribed');
    expect(button().disabled).toBe(true);
  });

  it("a site that does not take it shows the daemon's reason and keeps the button", async () => {
    const reason = 'one.example answered 500 and did not confirm, so you may still be subscribed. Try again later, or choose Always trash.';
    routes[UNSUB] = [502, { error: { code: 'unsubscribe_failed', message: reason } }];
    render(Senders);
    await screen.findByText('One Weekly');
    await fireEvent.click(button());
    await waitFor(() => expect(toast.text).toBe(reason));
    expect(button().textContent?.trim()).toBe('Unsubscribe');
    expect(button().disabled).toBe(false);
  });
});
