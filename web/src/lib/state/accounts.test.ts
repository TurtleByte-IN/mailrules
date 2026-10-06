import { expect, it } from 'vitest';
import * as accountsApi from '../api/accounts';
import { accounts, connect, load, reconnect, remove, test } from './accounts.svelte';
import { toast } from './toast.svelte';

it('adds, reconnects, tests and removes a mailbox', async () => {
  await load();
  expect(accounts.list.map((a) => a.email)).toEqual(['me@icloud.com', 'work@fastmail.com']);

  await connect({ preset: 'fastmail', email: 'new@fastmail.com', password: 'pw' });
  const added = accounts.list[2];
  expect(added).toMatchObject({ email: 'new@fastmail.com', provider: 'Fastmail', status: 'live' });
  expect(toast.text).toBe('new@fastmail.com is live');

  await reconnect(added.id);
  expect(accounts.list[2].lastEventAt).not.toBeNull();
  expect(toast.text).toBe('new@fastmail.com reconnected');

  await test(added.id);
  expect(toast.text).toBe('Connected. 14 folders found; Junk, Trash and Archive detected. Push (IDLE) supported.');

  await remove(added.id);
  expect(toast.text).toBe('new@fastmail.com removed');
  expect(accounts.list).toHaveLength(2);
  expect(await accountsApi.list()).toHaveLength(2);
});
