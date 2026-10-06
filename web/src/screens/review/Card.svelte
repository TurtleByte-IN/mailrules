<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { ActivityItem } from '../../lib/api/activity';
  import { clock, confidence } from '../../lib/format';
  import { resolve } from '../../lib/state/review.svelte';
  import { rules } from '../../lib/state/rules.svelte';
  import Always from '../activity/Always.svelte';

  let { item }: { item: ActivityItem } = $props();

  // The rule the model would have picked; null when it had none.
  const suggest = $derived(item.decision?.rule_id ?? null);
  const suggestName = $derived(item.decision?.rule_name || 'Inbox');
  let other = $derived(String(suggest ?? rules.list[0]?.id ?? ''));
  let always: Always;

  // ruleId null keeps it in the Inbox.
  const answer = async (ruleId: number | null) => always.settle(await resolve(item.id, ruleId, always.value()));
</script>

<article class="card flex flex-col gap-3 p-[18px]">
  <div class="flex flex-wrap items-baseline justify-between gap-2">
    <div class="min-w-0 break-words"><span class="font-semibold">{item.from_name || item.from}</span><span class="text-nav"> · {item.subject}</span></div>
    <span class="font-mono text-xs text-muted">{clock(item.received_at ?? item.created_at)}</span>
  </div>
  <p class="text-[13px] break-words text-nav">{item.snippet}</p>
  <div class="flex flex-wrap items-center gap-2 text-[13px]">
    {#if item.decision?.model}
      <span class="chip chip-review text-[13px]">Best guess: {suggestName} · {confidence(item.decision.confidence)}</span>
    {/if}
    <span class="text-muted">{item.decision?.reason}</span>
  </div>
  <div class="flex flex-wrap items-center gap-2">
    {#if suggest !== null}
      <button type="button" class="btn-primary" onclick={() => answer(suggest)}>Yes, {suggestName}</button>
    {/if}
    <button type="button" class="btn" onclick={() => answer(null)}>Keep in Inbox</button>
    <span class="flex min-w-0 flex-wrap items-center gap-1.5">
      <label for="other-{item.id}" class="text-[13px] text-secondary">or</label>
      <select id="other-{item.id}" class="field min-w-0" bind:value={other}>
        {#each rules.list as r (r.id)}
          <option value={String(r.id)}>{r.name}</option>
        {/each}
      </select>
      <button type="button" class="btn" disabled={!other} onclick={() => answer(Number(other))}>Apply</button>
    </span>
  </div>
  <div class="flex flex-wrap items-center gap-x-[18px] gap-y-2 border-t border-line-divider pt-2.5">
    <Always bind:this={always} {item} />
    <a href="/compose" use:link class="inline-flex min-h-8 items-center text-[13px] font-semibold text-ink no-underline">Create a rule for emails like this</a>
  </div>
</article>
