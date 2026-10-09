<script lang="ts">
  import type { ServerCert } from '../../lib/api/accounts';
  import { fullDay } from '../../lib/format';

  /**
   * A mail server's certificate for the person to check before accepting it: the daemon's
   * message saying why it is shown, and what to compare. Accepting pins this exact certificate
   * for the mailbox.
   */
  let { cert, message, busy = false, onaccept, oncancel }: { cert: ServerCert; message: string; busy?: boolean; onaccept: () => void; oncancel?: () => void } = $props();
</script>

<div role="alert" aria-label="Server certificate" class="flex flex-col gap-2.5 rounded-md border border-warn-line bg-warn-bg px-3.5 py-3 text-[13px]">
  <p class="text-warn">{message}</p>
  <dl class="m-0 grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1">
    <dt class="text-secondary">SHA-256 fingerprint</dt>
    <dd class="m-0 font-mono break-all">{cert.fingerprint}</dd>
    <dt class="text-secondary">Issued to</dt>
    <dd class="m-0 break-all">{cert.subject}</dd>
    <dt class="text-secondary">Issued by</dt>
    <dd class="m-0 break-all">{cert.issuer}</dd>
    <dt class="text-secondary">Valid</dt>
    <dd class="m-0">{fullDay(cert.not_before)} to {fullDay(cert.not_after)}</dd>
  </dl>
  <p class="text-secondary">Accepting trusts this certificate, and only it, for this mailbox. If the server ever presents another, MailRules stops and asks again.</p>
  <span class="flex flex-wrap justify-end gap-2">
    {#if oncancel}
      <button type="button" class="btn" onclick={oncancel}>Cancel</button>
    {/if}
    <button type="button" class="btn-primary" disabled={busy} onclick={onaccept}>{busy ? 'Accepting…' : 'Accept certificate'}</button>
  </span>
</div>
