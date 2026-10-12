<script lang="ts">
  import type { ModelUsage, StatsUsage } from '../lib/api/usage';
  import { day as shortDay, money } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { limits, load as loadLimits } from '../lib/state/limits.svelte';
  import { load, totals, usage } from '../lib/state/usage.svelte';

  load();
  loadLimits();

  type Day = StatsUsage['days'][number];

  // The prototype names three purposes, plus rule suggestions; any other shows as the API sends it.
  const purposes: Partial<Record<ModelUsage['purpose'], string>> = { decide: 'decision model', escalate: 'fallback', compose: 'rule composer', suggest: 'rule suggestions' };
  const n = (x: number) => x.toLocaleString();
  // Days are UTC dates, so they are formatted in UTC rather than shifted into the browser's zone.
  const date = (day: string) => new Date(day).toLocaleDateString([], { day: 'numeric', month: 'short', timeZone: 'UTC' });
  const tokens = (m: ModelUsage) => Math.round((m.tokens_in + m.tokens_out) / 1000);

  // One bar segment per model the range saw, in the order they first appear.
  const series = $derived([...new Set((usage.summary?.days ?? []).flatMap((d) => d.models.map((m) => m.model)))]);
  // ponytail: four fills; a fifth model repeats the first. Add a token when someone runs that many.
  const fills = ['bg-ink', 'bg-signal', 'bg-review', 'bg-trash'];
  const calls = (d: Day, model?: string) => d.models.reduce((a, m) => a + (model === undefined || m.model === model ? m.calls : 0), 0);
  // When the month does not fit (a phone), the chart scrolls sideways and opens on the latest days.
  const latest = (ul: HTMLElement) => {
    ul.scrollLeft = ul.scrollWidth;
  };
  // The first of next month is a UTC date, shown as such.
  const resets = (ts: number) => new Date(ts * 1000).toLocaleDateString([], { day: 'numeric', month: 'long', timeZone: 'UTC' });

  // The plan's meters, as the prototype's Plan screen draws them; none on self-host. Emails decided on the tenant's
  // own keys are not counted, so that meter shows no limit.
  const meters = $derived.by(() => {
    const l = limits.value;
    const out: { label: string; text: string; now: number; max: number }[] = [];
    if (l?.mailboxes) out.push({ label: 'Mailboxes', text: n(l.mailboxes.used) + ' of ' + n(l.mailboxes.limit), now: l.mailboxes.used, max: l.mailboxes.limit });
    const ai = l?.ai_emails;
    if (ai) {
      const counted = { text: n(ai.used) + ' of ' + n(ai.limit), now: ai.used, max: ai.limit };
      out.push({ label: 'AI-sorted emails this month', ...(ai.counted ? counted : { text: n(ai.used) + ' · no limit with your own keys', now: 0, max: 0 }) });
    }
    return out;
  });
</script>

<div class="flex min-w-0 flex-col gap-[18px]">
  <header>
    <h1>Usage</h1>
    <p class="mt-1 text-secondary">What sorting cost this month, and where the model calls went.</p>
  </header>

  {#if limits.value && meters.length}
    {@const l = limits.value}
    {@const ai = l.ai_emails}
    <section aria-label="Your plan" class="card flex flex-col gap-4 p-[18px]">
      <h2 class="text-[15px]">Your plan</h2>
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
      {#if ai?.paused}
        <p role="status" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">
          This month's AI-sorted emails are used up. Until {resets(ai.resets_at)}, email that needs AI waits in Needs review; sender rules and rules built from conditions keep sorting.
          <a href="#/settings" class="font-semibold underline">Add your own model key</a>{#if l.upgrade_url}{' or '}<a href={l.upgrade_url} class="font-semibold underline">upgrade</a>{/if} to have AI sort new mail again.
        </p>
      {:else if ai?.counted}
        <p class="text-[12.5px] text-muted">
          Counts start again on {resets(ai.resets_at)}. Sender rules, rules built from conditions and your own model keys are not counted.{#if l.upgrade_url}{' '}<a href={l.upgrade_url} class="underline">Upgrade</a>{/if}
        </p>
      {/if}
      {#if l.first_cleanup && l.first_cleanup.used < l.first_cleanup.limit}
        <p class="text-[12.5px] text-secondary">
          First cleanup: {n(l.first_cleanup.limit - l.first_cleanup.used)} of {n(l.first_cleanup.limit)} free AI-sorted emails left, used before this month's.
        </p>
      {/if}
    </section>
  {/if}

  {#if usage.status === 'error'}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
      <span>{usage.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {:else if usage.summary}
    {@const s = usage.summary}
    {@const t = totals(s)}
    {@const max = Math.max(1, ...s.days.map((d) => calls(d)))}
    {@const tiles = [
      { label: 'Emails acted on', value: n(t.emails), sub: 'since ' + shortDay(s.since) + ', ' + accounts.list.length + (accounts.list.length === 1 ? ' mailbox' : ' mailboxes') },
      { label: 'Decided for free', value: t.freePct + '%', sub: 'conditions and sender rules' },
      { label: 'Model calls', value: n(t.calls), sub: 'decision model ' + n(t.decide) + ' · fallback ' + n(t.escalate) },
      { label: 'Cost this month', value: money(t.costUsd), sub: '' },
    ]}
    <section class="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-3">
      {#each tiles as tile (tile.label)}
        <div class="card px-[18px] py-4">
          <div class="text-[12.5px] font-medium text-secondary">{tile.label}</div>
          <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{tile.value}</div>
          <div class="text-[12.5px] text-muted">{tile.sub}</div>
        </div>
      {/each}
    </section>

    <section aria-label="Model calls by day" class="card flex min-w-0 flex-col gap-3 p-[18px]">
      <div class="flex flex-wrap items-baseline justify-between gap-2">
        <h2 class="text-[15px]">Model calls per day</h2>
        <div class="flex flex-wrap gap-3.5 text-[12.5px] text-secondary">
          {#each series as model, i (model)}
            <span class="inline-flex items-center gap-1.5"><span class="size-2.5 rounded {fills[i % fills.length]}"></span>{model}</span>
          {/each}
        </div>
      </div>
      <ul class="relative flex h-40 items-end gap-1.5 overflow-x-auto" {@attach latest}>
        {#each s.days as d (d.day)}
          {@const text = date(d.day) + ': ' + (series.map((model) => model + ' ' + calls(d, model)).join(' · ') || 'no calls')}
          <li class="flex flex-[1_0_16px] flex-col items-center gap-1" title={text}>
            <span class="sr-only">{text}</span>
            <div class="flex h-[130px] w-full flex-col justify-end">
              <div class="flex flex-col-reverse overflow-hidden rounded-t" style:height="{(calls(d) / max) * 100}%">
                {#each series as model, i (model)}
                  <div class={fills[i % fills.length]} style:flex-grow={calls(d, model)}></div>
                {/each}
              </div>
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
            {#each s.by_rule as r (r.rule_id ?? r.rule_name)}
              <tr class="border-t border-line-divider">
                <td class="px-[18px] py-2.5 font-medium">{r.rule_id === null ? 'No rule' : r.rule_name}</td>
                <td class="px-2 py-2.5">{n(r.emails)}</td>
                <td class="px-2 py-2.5">{n(r.calls)}</td>
                <td class="px-[18px] py-2.5 text-right font-mono">{r.calls ? money(r.cost_usd) : 'Free'}</td>
              </tr>
            {:else}
              <tr class="border-t border-line-divider">
                <td colspan="4" class="px-[18px] py-2.5 text-[13px] text-muted">No emails decided yet.</td>
              </tr>
            {/each}
          </tbody>
        </table>
        <p class="border-t border-line-divider px-[18px] py-2.5 text-[12.5px] text-muted">Composing and testing rules are not counted per rule.</p>
      </section>

      <section aria-label="By model" class="card flex min-w-0 flex-[2_1_300px] flex-col gap-3 p-[18px]">
        <h2 class="text-[15px]">By model, this month</h2>
        {#each s.by_model as m (m.provider + m.model + m.purpose)}
          <div class="flex justify-between gap-2 border-b border-line-divider pb-2.5 text-[13.5px]">
            <span>
              <span class="font-semibold">{m.model} ({purposes[m.purpose] ?? m.purpose})</span><br />
              <span class="text-[12.5px] text-muted">{n(m.calls)} calls{tokens(m) ? ' · ' + tokens(m) + 'k tokens' : ''}</span>
            </span>
            <span class="font-mono">{money(m.cost_usd)}</span>
          </div>
        {/each}
        <div class="flex justify-between gap-2 border-b border-line-divider pb-2.5 text-[13.5px]">
          <span>
            <span class="font-semibold">Conditions and sender rules</span><br />
            <span class="text-[12.5px] text-muted">{n(s.without_model)} emails</span>
          </span>
          <span class="font-mono">Free</span>
        </div>
        <div class="text-[12.5px] text-secondary">Sender rules and conditions decided {t.freePct}% of email for free.</div>
      </section>
    </div>
  {/if}
</div>
