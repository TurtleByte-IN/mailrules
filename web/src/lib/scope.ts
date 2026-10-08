// The scope of a scan over mail already in a mailbox: which mailbox, which folder, which emails.
// Cleanup and "Suggest from my mail" pick it with the same controls (components/ScopePicker.svelte).
import type { CleanupCheckRequest, Folder } from './api/cleanup';
import { settings } from './state/settings.svelte';

/** The fields of a scan request a scope fills in: the mailbox, folder and range. */
export type ScopeRequest = { account_id: number } & Pick<CleanupCheckRequest, 'folder' | 'since' | 'limit'>;

/** What the scope controls hold; toRequest turns it into the contract's request fields. */
export interface Scope {
  accountId: string;
  /** The server's own folder name. */
  folder: string;
  /** The newest N emails, the emails from the last N days, or all mail. Every one is capped at the daemon's `limits.check_max`. */
  mode: 'newest' | 'days' | 'all';
  /** The "Newest emails" box; null is an empty box. */
  newest: number | null;
  /** The "From the last days" box; null is an empty box. */
  days: number | null;
}

/** The scope a screen starts on, before a mailbox is picked. The newest 200 is this screen's own first suggestion. */
export const startScope = (): Scope => ({ accountId: '', folder: 'INBOX', mode: 'newest', newest: 200, days: 90 });

/** What is wrong with the chosen number, in a sentence; empty when nothing is, and for "All mail", which has no box. */
export function scopeProblem(s: Scope) {
  if (s.mode === 'days') return s.days !== null && Number.isInteger(s.days) && s.days >= 1 ? '' : 'Give a whole number of days, 1 or more.';
  if (s.mode === 'all') return '';
  // Until the settings say the daemon's largest number, only a whole number from 1 is asked for; the daemon still checks.
  const max = settings.value.limits.check_max;
  if (s.newest !== null && Number.isInteger(s.newest) && s.newest >= 1 && (!max || s.newest <= max)) return '';
  return max ? `The limit must be between 1 and ${max}.` : 'Give a whole number of emails, 1 or more.';
}

// Every scan covers at most the newest `limits.check_max` emails of its range; a request with no limit
// gets that many, so only "Newest emails" sends one. A start before the epoch is the epoch: a huge
// number of days means all mail.
export function toRequest(s: Scope): ScopeRequest {
  const account_id = Number(s.accountId);
  if (s.mode === 'newest') return { account_id, folder: s.folder, since: null, limit: s.newest! };
  if (s.mode === 'days') return { account_id, folder: s.folder, since: Math.max(0, Math.floor(Date.now() / 1000) - s.days! * 86400) };
  return { account_id, folder: s.folder, since: null };
}

/** Archive goes by a different name on every server; the mailbox's folder list knows which. */
export const archiveFolder = (folders: Folder[]) => folders.find((f) => f.special_use === '\\Archive')?.name;
