import { expect, it, vi } from 'vitest';
import { mailboxReturn, setupReturn, startOneClick } from './oneclick';

it.each([
  ['', {}],
  ['added=7', { added: 7 }],
  ['reconnected=3', { reconnected: 3 }],
  ['mailbox_error=exists', { error: 'This mailbox is already connected.' }],
  ['mailbox_error=mismatch', { error: "You signed in as another address than this mailbox's. Reconnect, and sign in as the mailbox's own address." }],
  ['mailbox_error=something_new', { error: "That sign-in didn't go through, so no mailbox was connected. Try again." }],
  ['added=x', {}],
])('reads %j', (query, want) => {
  expect(JSON.parse(JSON.stringify(mailboxReturn(query)))).toEqual(want);
});

it('takes the answer to a sign-in started from setup back to setup, once', () => {
  vi.stubGlobal('location', { ...window.location, assign: vi.fn() });
  startOneClick('/api/oauth/start?provider=gmail', 'setup');
  expect(window.location.assign).toHaveBeenCalledWith('/api/oauth/start?provider=gmail');
  expect(setupReturn('#/accounts?added=7')).toBe('#/setup?added=7');
  // The note is used up: the next answer stays on Mailboxes.
  expect(setupReturn('#/accounts?added=8')).toBeUndefined();
  vi.unstubAllGlobals();
});

it.each([
  ['#/accounts?mailbox_error=exists', '#/setup?mailbox_error=exists'],
  ['#/accounts?reconnected=3', undefined],
  ['#/accounts', undefined],
  ['#/rules?added=7', undefined],
])('from setup, %s opens %s', (hash, want) => {
  sessionStorage.setItem('mailrules.oneclick.return', 'setup');
  expect(setupReturn(hash)).toBe(want);
  sessionStorage.clear();
});

it('leaves the answer to a sign-in started from Mailboxes on Mailboxes, even after one from setup was given up', () => {
  vi.stubGlobal('location', { ...window.location, assign: vi.fn() });
  startOneClick('/a', 'setup');
  startOneClick('/a', 'accounts');
  expect(setupReturn('#/accounts?added=7')).toBeUndefined();
  vi.unstubAllGlobals();
});
