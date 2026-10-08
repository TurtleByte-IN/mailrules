// Messages for ?signin_error=<code>, set by the daemon when a sign-in module's sign-in fails.

const refused = "That sign-in didn't go through. Try again.";

const messages: Record<string, string> = {
  unavailable: "We couldn't reach the sign-in service. Try again in a minute.",
  refused,
  choose_tenant: 'Your account belongs to more than one organisation. Choose one to continue.',
  tenant_mismatch: 'This account signed up under a different organisation. Sign in with that one.',
  email_in_use: 'Another MailRules account already uses this email address.',
};

/**
 * Reads ?signin_error from the address bar, removes it (keeping the rest of the URL) and
 * returns its message (a code it does not know reads as refused), or null when there is none.
 */
export function takeSignInError(): string | null {
  const url = new URL(window.location.href);
  const code = url.searchParams.get('signin_error');
  if (code === null) return null;
  url.searchParams.delete('signin_error');
  history.replaceState(history.state, '', url.pathname + url.search + url.hash);
  return Object.hasOwn(messages, code) ? messages[code] : refused;
}
