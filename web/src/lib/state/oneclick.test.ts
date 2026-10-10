import { expect, it } from 'vitest';
import { mailboxReturn } from './oneclick';

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
