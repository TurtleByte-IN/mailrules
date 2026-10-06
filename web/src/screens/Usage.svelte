<script lang="ts">
  import type { ModelUsage } from '../lib/api/usage';
  import { money } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import { load, totals, usage } from '../lib/state/usage.svelte';

  load();

  // The prototype names three purposes; any other shows as the API sends it.
  const purposes: Partial<Record<ModelUsage['purpose'], string>> = { decide: 'decision model', escalate: 'fallback', compose: 'rule composer' };
  const n = (x: number) => x.toLocaleString();
  // Days are UTC dates, so they are formatted in UTC rather than shifted into the browser's zone.
  const date = (day: string) => new Date(day).toLocaleDateString([], { day: 'numeric', month: 'short', timeZone: 'UTC' });
  const tokens = (m: ModelUsage) => Math.round((m.tokensIn + m.tokensOut) / 1000);
</script>

{#if usage.summary}
  {@const s = usage.summary}
  {@const t = totals(s)}
  {@const max = Math.max(1, ...s.days.map((d) => d.decide + d.escalate))}
  {@const tiles = [
    { label: 'Emails sorted', value: n(t.emails), sub: 'last 30 days, ' + accounts.list.length + ' mailboxes' },
    { label: 'Decided for free', value: t.freePct + '%', sub: 'conditions and sender rules' },
    { label: 'Model calls', value: n(t.calls), sub: 'Jev ' + n(t.decide) + ' · Haiku ' + n(t.escalate) },
    { label: 'Cost this month', value: money(t.costUsd), sub: 'of your ' + money(s.budgetUsd) + ' budget' },
  ]}
  <div class="flex flex-col gap-[18px]">
    <header>
      <h1>Usage</h1>
      <p class="mt-1 text-secondary">What sorting cost this month, and where the model calls went.</p>
    </header>

    <section class="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-3">
      {#each tiles as tile (tile.label)}
        <div class="card px-[18px] py-4">
          <div class="text-[12.5px] font-medium text-secondary">{tile.label}</div>
          <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{tile.value}</div>
          <div class="text-[12.5px] text-muted">{tile.sub}</div>
        </div>
      {/each}
    </section>

    <section aria-label="Model calls by day" class="card flex flex-col gap-3 p-[18px]">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <h2 class="text-[15px]">Model calls per day, last 14 days</h2>
        <div class="flex gap-3.5 text-[12.5px] text-secondary">
          <span class="inline-flex items-center gap-1.5"><span class="size-2.5 rounded bg-ink"></span>Jev</span>
          <span class="inline-flex items-center gap-1.5"><span class="size-2.5 rounded bg-signal"></span>Haiku fallback</span>
        </div>
      </div>
      <ul class="flex h-40 items-end gap-1.5 overflow-x-auto">
        {#each s.days as d (d.day)}
          {@const text = date(d.day) + ': Jev ' + d.decide + ' · Haiku ' + d.escalate}
          <li class="flex flex-[1_0_28px] flex-col items-center gap-1" title={text}>
            <span class="sr-only">{text}</span>
            <div class="flex h-[130px] w-full flex-col justify-end">
              <div class="rounded-t bg-signal" style:height="{(d.escalate / max) * 100}%"></div>
              <div class="bg-ink" style:height="{(d.decide / max) * 100}%"></div>
            </div>
            <span class="text-[11px] text-muted" aria-hidden="true">{Number(d.day.slice(8))}</span>
          </li>
        {/each}
      </ul>
    </section>

    <div class="flex flex-wrap items-start gap-[18px]">
      <section aria-label="By rule" class="card min-w-0 flex-[3_1_460px] overflow-x-auto">
        <table class="w-full border-collapse text-[13.5px]">
          <caption class="px-[18px] py-3.5 text-left text-[15px] font-semibold">By rule, this month</caption>
          <thead>
            <tr class="text-left text-[12.5px] text-muted">
              <th class="px-[18px] py-2 font-medium">Rule</th>
              <th class="p-2 font-medium">Emails</th>
              <th class="p-2 font-medium">Model calls</th>
              <th class="px-[18px] py-2 text-right font-medium">Cost</th>
            </tr>
          </thead>
          <tbody>
            {#each s.rules as r (r.ruleId)}
              <tr class="border-t border-line-divider">
                <td class="px-[18px] py-2.5 font-medium">{rules.list.find((x) => x.id === r.ruleId)?.name ?? r.ruleId}</td>
                <td class="px-2 py-2.5">{n(r.emails)}</td>
                <td class="px-2 py-2.5">{n(r.calls)}</td>
                <td class="px-[18px] py-2.5 text-right font-mono">{r.calls ? money(r.costUsd) : 'Free'}</td>
              </tr>
            {/each}
          </tbody>
        </table>
      </section>

      <section aria-label="By model" class="card flex min-w-0 flex-[2_1_300px] flex-col gap-3 p-[18px]">
        <h2 class="text-[15px]">By model, this month</h2>
        {#each s.models as m (m.model + m.purpose)}
          <div class="flex justify-between gap-2 border-b border-line-divider pb-2.5 text-[13.5px]">
            <span>
              <span class="font-semibold">{m.model} ({purposes[m.purpose] ?? m.purpose})</span><br />
              <span class="text-[12.5px] text-muted">{n(m.calls)} calls{tokens(m) ? ' · ' + tokens(m) + 'k tokens' : ''}</span>
            </span>
            <span class="font-mono">{money(m.costUsd)}</span>
          </div>
        {/each}
        <div class="flex justify-between gap-2 border-b border-line-divider pb-2.5 text-[13.5px]">
          <span>
            <span class="font-semibold">Conditions and sender rules</span><br />
            <span class="text-[12.5px] text-muted">{n(s.freeEmails)} emails</span>
          </span>
          <span class="font-mono">Free</span>
        </div>
        <div class="text-[12.5px] text-secondary">Sender rules and conditions decided {t.freePct}% of email for free.</div>
      </section>
    </div>
  </div>
{/if}
