<script lang="ts">
  /**
   * What MailRules does with a Gmail mailbox, shown before every Google sign-in MailRules starts: adding one
   * with one click, or reconnecting one. `oncontinue` leaves for Google; `onback` returns to where it was.
   */
  let { oncontinue, onback, again = false }: { oncontinue: () => void; onback: () => void; again?: boolean } = $props();
</script>

<section aria-label="Before you sign in to Google" class="flex flex-col gap-3">
  <h2>Before you {again ? 'sign in to Google again' : 'sign in to Google'}</h2>
  <dl class="m-0 grid gap-2.5 text-[13.5px] text-nav">
    <div>
      <dt class="font-semibold text-ink">What MailRules reads</dt>
      <dd class="m-0">
        Each new email's headers and the start of its text, without marking it read. It keeps sender, recipients, subject, List-Id, time, size,
        whether there are attachments, and a 200-character snippet (blanked after 30 days by default); never the full email. From Sent Mail it reads
        only the To and Cc of your newest 2,000 messages, to know who you write to.
      </dd>
    </div>
    <div>
      <dt class="font-semibold text-ink">What it does</dt>
      <dd class="m-0">
        Moves mail to labels, archives, moves to trash (a "MailRules Trash" label by default, never emptied), marks junk, flags, and marks read or
        unread. It never sends email and never deletes anything for good. Every change is logged and can be undone for 30 days. New mailboxes start in
        dry-run, so nothing changes until you turn it off.
      </dd>
    </div>
    <div>
      <dt class="font-semibold text-ink">What leaves MailRules</dt>
      <dd class="m-0">
        Only for an email a plain-English rule must decide: the AI model provider gets the sender, whether you wrote to them, recipients, subject,
        mailing-list signals, attachment types (never attachments), the first 2,000 characters of the text, and the rules' names and descriptions.
        Notifications you set up get mailbox, rule, actions, folder, sender and subject; the summary email gets senders and subjects.
      </dd>
    </div>
    <div>
      <dt class="font-semibold text-ink">How to remove access</dt>
      <dd class="m-0">
        Mailboxes → Remove deletes the mailbox's token, folder list, contacts, activity and undo history at once, and changes nothing in Gmail. You can
        also revoke access in your Google Account, under Security → third-party connections.
      </dd>
    </div>
  </dl>
  <p class="text-[12.5px] text-secondary">
    MailRules' use and transfer of information received from Google APIs will adhere to the Google API Services User Data Policy, including the
    Limited Use requirements. <a href="https://mailrules.app/privacy" target="_blank" rel="noreferrer" class="font-semibold">Privacy policy</a>
  </p>
  <div class="flex flex-wrap justify-between gap-2 border-t border-line-divider pt-3.5">
    <button type="button" class="btn min-h-11" onclick={onback}>Back</button>
    <button type="button" class="btn-primary min-h-11 px-[18px]" onclick={oncontinue}>Continue to Google</button>
  </div>
</section>
