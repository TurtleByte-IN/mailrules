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

// The daemon always sends the browser back to #/accounts (ext.Host.MailboxAdded/MailboxFailed). A sign-in
// started from first-run setup leaves a note in this tab's session storage, which survives the trip to the
// provider, so the app takes the answer on to /setup instead.
const hintKey = 'mailrules.oneclick.return';

/** Leaves for the provider's sign-in at `url`, noting where the answer should go: first-run setup or Mailboxes. */
export function startOneClick(url: string, from: 'setup' | 'accounts') {
  try {
    if (from === 'setup') sessionStorage.setItem(hintKey, 'setup');
    else sessionStorage.removeItem(hintKey);
  } catch {
    // No session storage: the answer lands on Mailboxes, which also goes on at Rules.
  }
  window.location.assign(url);
}

/**
 * For the address the app opens at (`hash`, as location.hash): when it is a one-click answer
 * (#/accounts?…) to a sign-in started from setup, the same answer at #/setup; otherwise undefined.
 * The note is used once.
 */
export function setupReturn(hash: string): string | undefined {
  const m = /^#\/accounts\?(.+)$/.exec(hash);
  if (!m) return undefined;
  const r = mailboxReturn(m[1]);
  if (!r.added && !r.error) return undefined;
  try {
    const from = sessionStorage.getItem(hintKey);
    sessionStorage.removeItem(hintKey);
    return from === 'setup' ? '#/setup?' + m[1] : undefined;
  } catch {
    return undefined;
  }
}
