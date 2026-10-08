<script lang="ts">
  import { previewSummary, sendTestSummary, type SummaryPatch, type SummaryPreview } from '../../lib/api/settings';
  import { clock, weekday } from '../../lib/format';
  import { patch, settings } from '../../lib/state/settings.svelte';

  const sum = $derived(settings.value.summary);
  // Without a mail server the switch can only be turned off.
  const locked = $derived(!sum.smtp.configured && !sum.enabled);
  const days = ['monday', 'tuesday', 'wednesday', 'thursday', 'friday', 'saturday', 'sunday'] as const;
  const missing = $derived(sum.smtp.missing.length > 1 ? sum.smtp.missing.slice(0, -1).join(', ') + ' and ' + sum.smtp.missing.at(-1) : sum.smtp.missing[0] ?? '');
  const when = (ts: number) => weekday(ts) + ', ' + clock(ts);

  let sent = $state<{ ok: boolean; text: string } | null>(null);
  let sending = $state(false);
  let preview = $state<SummaryPreview | null>(null);
  let previewError = $state('');
  let showing = $state(false);

  const reason = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

  /** Saves one field with the browser's time zone, so `time` means the user's clock; resolves false when refused. */
  const save = (p: SummaryPatch) => patch({ summary: { ...p, time_zone: Intl.DateTimeFormat().resolvedOptions().timeZone } });

  async function saveEnabled(e: Event & { currentTarget: HTMLInputElement }) {
    const el = e.currentTarget;
    if (!(await save({ enabled: el.checked }))) el.checked = sum.enabled;
  }

  async function saveField(e: Event & { currentTarget: HTMLInputElement | HTMLSelectElement }, name: 'frequency' | 'weekday' | 'time' | 'to') {
    const el = e.currentTarget;
    const value = el.value.trim();
    const p: SummaryPatch = name === 'to' ? { to: value || null } : ({ [name]: value } as SummaryPatch);
    // `to` comes back as the address in force; the box shows it only when one was saved.
    if (!(await save(p))) el.value = name === 'to' ? shownTo : sum[name];
  }
  // The admin's address is the placeholder; the box holds an address only when another was chosen.
  const shownTo = $derived(sum.to === sum.to_default ? '' : sum.to);

  async function test() {
    sending = true;
    sent = null;
    try {
      const r = await sendTestSummary();
      sent = { ok: true, text: `Sent to ${r.to}.` };
    } catch (e) {
      sent = { ok: false, text: reason(e) };
    } finally {
      sending = false;
    }
  }

  async function togglePreview() {
    showing = !showing;
    if (!showing) return;
    previewError = '';
    try {
      preview = await previewSummary();
    } catch (e) {
      preview = null;
      previewError = reason(e);
    }
  }
</script>

<section aria-label="Summary email" class="card flex flex-col gap-3.5 p-5">
  <h2>Summary email</h2>
  <label class="flex items-center gap-2.5 {locked ? 'cursor-not-allowed opacity-60' : 'cursor-pointer'}">
    <input type="checkbox" checked={sum.enabled} disabled={locked} onchange={saveEnabled} />
    <span><span class="font-semibold">Send me a summary</span><span class="text-secondary">{' · '}what was sorted, trashed and needs review, with links to fix each</span></span>
  </label>
  <div class="text-[12.5px] text-secondary">Free. Sent through your own mail server (the MAILRULES_SMTP_* settings).</div>
  {#if !sum.smtp.configured}
    <div class="text-[13px] text-review">To send it, set {missing || 'the MAILRULES_SMTP_* settings'}, then restart MailRules.</div>
  {/if}

  <div class="flex flex-wrap gap-3">
    <div class="flex flex-col gap-1.5">
      <label for="sum-freq" class="text-[13px] font-semibold">How often</label>
      <select id="sum-freq" class="field h-11 px-2.5" value={sum.frequency} onchange={(e) => saveField(e, 'frequency')}>
        <option value="daily">Every day</option>
        <option value="weekly">Every week</option>
      </select>
    </div>
    {#if sum.frequency === 'weekly'}
      <div class="flex flex-col gap-1.5">
        <label for="sum-day" class="text-[13px] font-semibold">On</label>
        <select id="sum-day" class="field h-11 px-2.5" value={sum.weekday} onchange={(e) => saveField(e, 'weekday')}>
          {#each days as d (d)}<option value={d}>{d[0].toUpperCase() + d.slice(1)}</option>{/each}
        </select>
      </div>
    {/if}
    <div class="flex flex-col gap-1.5">
      <label for="sum-time" class="text-[13px] font-semibold">At</label>
      <input id="sum-time" type="time" step="60" class="field h-11 px-2.5" value={sum.time} onchange={(e) => saveField(e, 'time')} />
    </div>
    <div class="flex min-w-0 flex-[1_1_220px] flex-col gap-1.5">
      <label for="sum-to" class="text-[13px] font-semibold">Send to</label>
      <input id="sum-to" type="email" autocomplete="email" class="field h-11 px-2.5" placeholder={sum.to_default} value={shownTo} onchange={(e) => saveField(e, 'to')} />
    </div>
  </div>

  {#if sum.enabled && sum.next_at}
    <div class="text-[12.5px] text-secondary">Next one: {when(sum.next_at)}{sum.last_sent_at ? ` · last sent ${when(sum.last_sent_at)}` : ''}</div>
  {:else if sum.last_sent_at}
    <div class="text-[12.5px] text-secondary">Last sent {when(sum.last_sent_at)}</div>
  {/if}

  <div class="flex flex-wrap gap-2">
    <button type="button" class="btn min-h-11" disabled={!sum.smtp.configured || sending} onclick={test}>Send a test email</button>
    <button type="button" class="btn min-h-11" aria-expanded={showing} onclick={togglePreview}>{showing ? 'Hide preview' : 'Show preview'}</button>
  </div>
  {#if sent}
    <div role={sent.ok ? 'status' : 'alert'} class="text-[13px] {sent.ok ? 'text-secondary' : 'text-trash'}">{sent.text}</div>
  {/if}

  {#if showing}
    {#if previewError}
      <div role="alert" class="text-[13px] text-trash">{previewError}</div>
    {:else if preview}
      <div class="flex flex-col gap-2">
        <div class="text-[13px] text-secondary">Subject: <strong class="text-ink">{preview.subject}</strong></div>
        {#if preview.dry_run}
          <div class="text-[12.5px] text-secondary">Dry-run is on, so the summary lists what MailRules recorded, not what it did.</div>
        {/if}
        <!-- A page of its own, not srcdoc: a srcdoc frame takes on the app's policy, which blocks the email's inline styles. -->
        <iframe title="Summary email preview" sandbox="allow-popups allow-popups-to-escape-sandbox" src={'/api/summary/preview.html?at=' + preview.period_end} class="h-[480px] w-full rounded border border-line-card"></iframe>
      </div>
    {/if}
  {/if}
</section>
