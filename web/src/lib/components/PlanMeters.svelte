<script lang="ts">
  import type { Limits } from '../api/limits';

  let { limits }: { limits: Limits } = $props();

  const n = (x: number) => x.toLocaleString();

  // The plan's meters, as the prototype's Plan screen draws them; none on self-host. Emails decided on the
  // tenant's own keys are not counted, so that meter shows no limit.
  const meters = $derived.by(() => {
    const l = limits;
    const out: { label: string; text: string; now: number; max: number }[] = [];
    if (l.mailboxes) out.push({ label: 'Mailboxes', text: n(l.mailboxes.used) + ' of ' + n(l.mailboxes.limit), now: l.mailboxes.used, max: l.mailboxes.limit });
    const ai = l.ai_emails;
    if (ai) {
      const counted = { text: n(ai.used) + ' of ' + n(ai.limit), now: ai.used, max: ai.limit };
      out.push({ label: 'AI-sorted emails this month', ...(ai.counted ? counted : { text: n(ai.used) + ' · no limit with your own keys', now: 0, max: 0 }) });
    }
    return out;
  });
</script>

<div class="grid grid-cols-[repeat(auto-fit,minmax(240px,1fr))] gap-[18px]">
  {#each meters as m (m.label)}
    <div class="flex flex-col gap-1.5">
      <div class="flex justify-between gap-2 text-[13.5px]"><span class="font-semibold">{m.label}</span><span class="text-secondary">{m.text}</span></div>
      <div role="meter" aria-label={m.label} aria-valuemin={0} aria-valuemax={m.max} aria-valuenow={m.now} aria-valuetext={m.text} class="h-2.5 overflow-hidden rounded-[3px] bg-neutral">
        <div class="h-full bg-ink" style:width="{m.max ? Math.min(100, Math.round((m.now / m.max) * 100)) : 0}%"></div>
      </div>
    </div>
  {/each}
</div>
