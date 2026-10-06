<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { ReviewItem } from '../../lib/api/review';
  import { clock, confidence } from '../../lib/format';
  import { resolve } from '../../lib/state/review.svelte';
  import { rules } from '../../lib/state/rules.svelte';

  let { item }: { item: ReviewItem } = $props();

  // svelte-ignore state_referenced_locally
  let other = $state(item.suggest);
  let always = $state(false);
  const suggestName = $derived(rules.list.find((r) => r.id === item.suggest)?.name ?? 'Inbox');
</script>

<article class="card flex flex-col gap-3 p-[18px]">
  <div class="flex flex-wrap items-baseline justify-between gap-2">
    <div class="min-w-0 break-words"><span class="font-semibold">{item.sender}</span><span class="text-nav"> · {item.subject}</span></div>
    <span class="font-mono text-xs text-muted">{clock(item.receivedAt)}</span>
  </div>
  <p class="text-[13px] break-words text-nav">{item.snippet}</p>
  <div class="flex flex-wrap items-center gap-2 text-[13px]">
    <span class="chip chip-review text-[13px]">Best guess: {suggestName} · {confidence(item.confidence)}</span>
    <span class="text-muted">{item.reason}</span>
  </div>
  <div class="flex flex-wrap items-center gap-2">
    <button type="button" class="btn-primary" onclick={() => resolve(item.id, item.suggest, always)}>Yes, {suggestName}</button>
    <button type="button" class="btn" onclick={() => resolve(item.id, null, always)}>Keep in Inbox</button>
    <span class="flex min-w-0 flex-wrap items-center gap-1.5">
      <label for="other-{item.id}" class="text-[13px] text-secondary">or</label>
      <select id="other-{item.id}" class="field min-w-0" bind:value={other}>
        {#each rules.list as r (r.id)}
          <option value={r.id}>{r.name}</option>
        {/each}
      </select>
      <button type="button" class="btn" onclick={() => resolve(item.id, other, always)}>Apply</button>
    </span>
  </div>
  <div class="flex flex-wrap items-center gap-x-[18px] gap-y-2 border-t border-line-divider pt-2.5">
    <label class="flex min-h-8 items-center gap-2 text-[13px] text-nav">
      <input type="checkbox" bind:checked={always} />Always do this for {item.domain}
    </label>
    <a href="/compose?idea={encodeURIComponent(item.idea)}" use:link class="inline-flex min-h-8 items-center text-[13px] font-semibold text-ink no-underline">Create a rule for emails like this</a>
  </div>
</article>
