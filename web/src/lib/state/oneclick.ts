// Where a one-click sign-in sends the browser back to: #/accounts?added=<id> (the wizard
// goes on at Rules), ?reconnected=<id>, or ?mailbox_error=<code> when it failed. The
// codes are ext.Mailbox* in the daemon.

const failed = "That sign-in didn't go through, so no mailbox was connected. Try again.";

const messages: Record<string, string> = {
  refused: failed,
  unavailable: "We couldn't reach the provider's sign-in. Try again in a minute.",
  exists: 'This mailbox is already connected.',
  mismatch: "You signed in as another address than this mailbox's. Reconnect, and sign in as the mailbox's own address.",
  connect_failed: "The mail server didn't accept the sign-in, so the mailbox wasn't connected. Check that IMAP is switched on for the account, then try again.",
};

export interface MailboxReturn {
  /** The mailbox just added: the wizard opens at its Rules step for it. */
  added?: number;
  /** The mailbox just signed in again. */
  reconnected?: number;
  /** Why the sign-in failed, as a sentence to show (a code it does not know reads as refused). */
  error?: string;
}

/** Reads what a one-click sign-in came back with from the route's query string. */
export function mailboxReturn(query: string): MailboxReturn {
  const q = new URLSearchParams(query);
  const code = q.get('mailbox_error');
  return {
    added: Number(q.get('added')) || undefined,
    reconnected: Number(q.get('reconnected')) || undefined,
    error: code === null ? undefined : Object.hasOwn(messages, code) ? messages[code] : failed,
  };
}
