// The scope of a scan over mail already in a mailbox: which mailbox, which folder, which emails.
// Cleanup and "Suggest from my mail" pick it with the same controls (components/ScopePicker.svelte).
import type { CleanupCheckRequest, Folder } from './api/cleanup';
import { TEST_LIMIT } from './api/rules';

/** The most emails one scan covers: the daemon's own bound, the same number the rule tester allows. */
export const CHECK_MAX = TEST_LIMIT.max;

/** What the scope controls hold; toRequest turns it into the contract's request fields. */
export interface Scope {
  accountId: string;
  /** The server's own folder name. */
  folder: string;
  /** The newest N emails, the emails from the last N days, or all mail. Every one is capped at CHECK_MAX. */
  mode: 'newest' | 'days' | 'all';
  /** The "Newest emails" box; null is an empty box. */
  newest: number | null;
  /** The "From the last days" box; null is an empty box. */
  days: number | null;
}

/** The scope a screen starts on, before a mailbox is picked. */
export const startScope = (): Scope => ({ accountId: '', folder: 'INBOX', mode: 'newest', newest: TEST_LIMIT.default, days: 90 });

/** What is wrong with the chosen number, in a sentence; empty when nothing is, and for "All mail", which has no box. */
export function scopeProblem(s: Scope) {
  if (s.mode === 'newest') return s.newest !== null && Number.isInteger(s.newest) && s.newest >= 1 && s.newest <= CHECK_MAX ? '' : `The limit must be between 1 and ${CHECK_MAX}.`;
  if (s.mode === 'days') return s.days !== null && Number.isInteger(s.days) && s.days >= 1 ? '' : 'Give a whole number of days, 1 or more.';
  return '';
}

// Every scan covers at most the newest CHECK_MAX emails of its range, so the limit is always sent.
// A start before the epoch is the epoch: a huge number of days means all mail.
export function toRequest(s: Scope): CleanupCheckRequest {
  const account_id = Number(s.accountId);
  if (s.mode === 'newest') return { account_id, folder: s.folder, since: null, limit: Math.min(s.newest!, CHECK_MAX) };
  if (s.mode === 'days') return { account_id, folder: s.folder, since: Math.max(0, Math.floor(Date.now() / 1000) - s.days! * 86400), limit: CHECK_MAX };
  return { account_id, folder: s.folder, since: null, limit: CHECK_MAX };
}

/** Archive goes by a different name on every server; the mailbox's folder list knows which. */
export const archiveFolder = (folders: Folder[]) => folders.find((f) => f.special_use === '\\Archive')?.name;
