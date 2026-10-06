import { afterEach, beforeAll, expect, it, vi } from 'vitest';
import * as accountsApi from '../../lib/api/accounts';
import { ApiError } from '../../lib/api/client';
import { accounts, load, loadPresets } from '../../lib/state/accounts.svelte';
import { Wizard } from './connect.svelte';

const SECRET = 'abcd-efgh-ijkl-mnop';

beforeAll(() => Promise.all([load(), loadPresets()]));
afterEach(() => vi.restoreAllMocks());

function atSignIn(fields: Partial<Pick<Wizard, 'presetId' | 'email' | 'password' | 'host'>> = {}) {
  const w = new Wizard();
  Object.assign(w, { step: 1, ...fields });
  return w;
}

it.each([
  ['no email', { password: SECRET }],
  ['no password', { email: 'a@icloud.com' }],
  ['blank password', { email: 'a@icloud.com', password: '   ' }],
  ['other IMAP without a host', { presetId: 'generic', email: 'a@example.com', password: SECRET }],
])('stays on sign-in with %s', async (_name, fields) => {
  const w = atSignIn(fields);
  expect(await w.next()).toBe(false);
  expect(w.step).toBe(1);
  expect(w.test).toBe('err');
  expect(w.error).toBe('Enter your email and the app-specific password first.');
});

it('does not leave the provider step on a sign-in method that is not built', async () => {
  const w = new Wizard();
  w.presetId = 'oauth';
  await w.next();
  expect(w.step).toBe(0);
});

it('tests first, then walks to the end and saves the mailbox', async () => {
  const w = new Wizard();
  const labels: string[] = [];
  const press = async () => {
    labels.push(w.nextLabel);
    return w.next();
  };

  await press();
  w.email = ' new@icloud.com ';
  w.password = SECRET;
  await press(); // runs the test, stays
  expect([w.step, w.test]).toEqual([1, 'ok']);
  await press();
  w.templates[0].on = false;
  w.templates[1].on = false;
  await press();
  expect(await press()).toBe(true);

  expect(labels).toEqual(['Continue', 'Test and continue', 'Continue', 'Preview with 1 rule', 'Go live']);
  expect(accounts.list.at(-1)).toMatchObject({ email: 'new@icloud.com', provider: 'iCloud', status: 'live' });
  expect((await accountsApi.list()).at(-1)?.email).toBe('new@icloud.com');
});

it('keeps the password out of state, the API data and browser storage', async () => {
  const w = atSignIn({ email: 'secret@icloud.com', password: SECRET });
  await w.next();
  w.step = 3;
  expect(await w.next()).toBe(true);

  expect(w.password).toBe('');
  expect(JSON.stringify([accounts, await accountsApi.list(), { ...localStorage }, { ...sessionStorage }])).not.toContain(SECRET);
});

it('voids a passed test when a sign-in field changes, and will not save without one', async () => {
  const w = atSignIn({ email: 'b@icloud.com', password: SECRET });
  await w.runTest();
  w.edited();
  expect(w.test).toBe('idle');

  w.step = 3;
  const before = accounts.list.length;
  expect(await w.next()).toBe(false);
  expect(accounts.list.length).toBe(before);
});

it('ignores a test result that arrives after the fields changed', async () => {
  const w = atSignIn({ email: 'c@icloud.com', password: SECRET });
  const running = w.runTest();
  w.edited();
  await running;
  expect(w.test).toBe('idle');
});

it('shows the daemon message when the test fails', async () => {
  vi.spyOn(accountsApi, 'test').mockRejectedValue(new ApiError(422, 'auth_failed', 'The server rejected that password.'));
  const w = atSignIn({ email: 'd@icloud.com', password: SECRET });
  expect(await w.next()).toBe(false);
  expect([w.step, w.test, w.error]).toEqual([1, 'err', 'The server rejected that password.']);
});
