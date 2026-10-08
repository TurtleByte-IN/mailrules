/**
 * Said before every undo that moves many emails. Undo runs with dry-run on: it reverses only what
 * MailRules really did on the mail server, so there is nothing for dry-run to hold back.
 */
export const UNDO_IGNORES_DRY_RUN = 'Dry-run does not stop an undo: it only reverses changes MailRules really made.';
