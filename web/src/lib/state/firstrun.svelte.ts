import { push } from 'svelte-spa-router';

/** The guide's steps after the admin account: the AI, the first mailbox, then a closing word. */
export type Step = 'ai' | 'mailbox' | 'done';

/** The step list as the guide shows it; the admin account is always behind the user. */
export const stepNames = ['Admin account', 'Decision model', 'First mailbox', 'Done'];
const order: Step[] = ['ai', 'mailbox', 'done'];

/**
 * The first-run guide at /setup. `offered` is set when the admin account is created in this
 * browser and settled once the mailboxes are listed: the guide opens only when there are none.
 * Signing in to an existing install never offers it. `step` is null while the guide is closed.
 */
export const firstRun = $state<{ offered: boolean; step: Step | null }>({ offered: false, step: null });

/** Called once the admin account has been created here. */
export function offer() {
  firstRun.offered = true;
}

/** Decides an offer once the mailboxes are listed: an install that already has one goes without. */
export function settle(mailboxes: number) {
  if (!firstRun.offered) return;
  firstRun.offered = false;
  if (mailboxes > 0) return;
  firstRun.step = 'ai';
  push('/setup');
}

/** Moves on to the next step. */
export function next() {
  if (firstRun.step) firstRun.step = order[order.indexOf(firstRun.step) + 1] ?? null;
}

/** Skips the step: the AI step moves on to the mailbox; the mailbox step leaves for Overview. */
export function skip() {
  if (firstRun.step === 'ai') next();
  else close('/');
}

/** Closes the guide and opens `to`. */
export function close(to: string) {
  firstRun.step = null;
  push(to);
}

/** Drops the guide on sign-out, so the next sign-in starts without it. */
export function forget() {
  firstRun.offered = false;
  firstRun.step = null;
}
