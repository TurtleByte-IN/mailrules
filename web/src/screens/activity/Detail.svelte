<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { MessageDetail, TraceStep } from '../../lib/api/activity';
  import { clock, confidence, money } from '../../lib/format';
  import { accounts } from '../../lib/state/accounts.svelte';
  import { correct } from '../../lib/state/activity.svelte';
  import { rules } from '../../lib/state/rules.svelte';

  // id keeps the select's label unique: the screen renders this panel once beside the feed and once under the row.
  let { message, id }: { message: MessageDetail; id: string } = $props();

  let correctTo = $state('keep');
  let always = $state(false);
  const account = $derived(accounts.list.find((a) => String(a.id) === String(message.account_id))?.label);

  // What a decision step picked and what it cost: "Recruiters 0.91 · 412 tokens · $0.0001 · 380 ms".
  const meta = (s: TraceStep) =>
    s.kind === 'action' || s.kind === 'correction'
      ? ''
      : [
          s.confidence !== null && `${s.rule_name || 'none'} ${confidence(s.confidence)}`,
          `${(s.tokens_in + s.tokens_out).toLocaleString()} tokens`,
          s.cost_usd > 0 && money(s.cost_usd),
          s.latency_ms > 0 && `${s.latency_ms} ms`,
        ]
          .filter(Boolean)
          .join(' · ');

  async function fix() {
    await correct(message.id, correctTo === 'keep' ? null : Number(correctTo), always);
    always = false;
  }
</script>

<div>
  <div class="text-xs font-medium tracking-[0.06em] text-muted uppercase">Why this happened</div>
  <h2 class="mt-1.5 text-base">{message.subject}</h2>
  <div class="mt-1 text-[13px] break-words text-secondary">
    {[message.from, clock(message.received_at ?? message.created_at), account].filter(Boolean).join(' · ')}
  </div>
</div>
<ol class="flex flex-col gap-3.5">
  {#each message.trace as step, i (i)}
    <li class="flex gap-3">
      <span class="grid size-[22px] shrink-0 place-items-center rounded-sm text-xs font-semibold {step.active ? 'bg-ink text-surface' : 'bg-neutral text-secondary'}">{i + 1}</span>
      <div class="min-w-0">
        <div class="font-semibold">{step.label}{step.model ? ` (${step.model})` : ''}</div>
        <div class="text-xs text-muted">{step.detail}</div>
        {#if meta(step)}<div class="mt-0.5 font-mono text-xs text-muted">{meta(step)}</div>{/if}
      </div>
    </li>
  {/each}
</ol>
<div class="rounded border border-line-divider bg-selected-row p-3">
  <div class="mb-1.5 text-xs text-muted">Preview (snippet kept for 30 days)</div>
  <p class="text-[13px] break-words text-nav">{message.snippet}</p>
</div>
{#if message.state === 'review'}
  <a href="/review" use:link class="btn-primary no-underline">Review</a>
{:else}
  <div class="flex flex-col gap-2.5">
    <label for="{id}-rule" class="text-[13px] font-semibold">Wrong call? Put it where it belongs</label>
    <div class="flex flex-wrap gap-2">
      <select id="{id}-rule" class="field min-w-0 flex-[1_1_160px]" bind:value={correctTo}>
        <option value="keep">Keep in Inbox</option>
        {#each rules.list as r (r.id)}
          <option value={String(r.id)}>{r.name}</option>
        {/each}
      </select>
      <button type="button" class="btn-primary" onclick={fix}>Fix it</button>
    </div>
    <label class="flex min-h-8 items-center gap-2 text-[13px] text-nav">
      <input type="checkbox" bind:checked={always} />Always do this for {message.from}
    </label>
  </div>
{/if}
