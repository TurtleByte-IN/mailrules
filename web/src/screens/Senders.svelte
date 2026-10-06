<script lang="ts">
  import { features } from '../lib/features';
  import type { Sender, Sort } from '../lib/api/senders';
  import { rules } from '../lib/state/rules.svelte';
  import { forget, load, more, nameOf, routingOf, senders, setQuery, setRouting, setSort, targetOf } from '../lib/state/senders.svelte';

  load();

  let query = $state('');

  async function route(s: Sender, select: HTMLSelectElement) {
    if (!(await setRouting(s, select.value))) select.value = routingOf(s);
  }
</script>

<div class="flex flex-col gap-[18px]">
  <header>
    <h1>Senders</h1>
    <p class="mt-1 text-secondary">Who emails you most. Set a sender once and their mail never needs a model again.</p>
  </header>

  {#if senders.status === 'error'}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
      <span>{senders.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {:else if senders.status === 'ready'}
    <div class="flex flex-wrap gap-2">
      <label class="field flex max-w-[420px] flex-[1_1_260px] items-center gap-2 text-muted focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-signal">
        <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
          <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
        </svg>
        <span class="sr-only">Search senders</span>
        <input type="search" class="w-full border-0 bg-transparent text-ink outline-none" placeholder="Search senders" bind:value={query} oninput={() => setQuery(query)} />
      </label>
      <label class="sr-only" for="sd-sort">Sort</label>
      <select id="sd-sort" class="field px-2.5" value={senders.sort} onchange={(e) => setSort(e.currentTarget.value as Sort)}>
        <option value="volume">Most email first</option>
        <option value="recent">Most recent first</option>
      </select>
    </div>

    <section aria-label="Sender list" class="card overflow-hidden">
      {#each senders.list as s, i (s.type + s.value)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-2.5 px-[18px] py-3.5 {i ? 'border-t border-line-divider' : ''}">
          <div class="min-w-0 flex-[1_1_220px]">
            <div class="font-semibold">{nameOf(s)}</div>
            <div class="font-mono text-[12.5px] text-muted">{s.type === 'domain' ? 'Domain: all its addresses' : s.value}</div>
          </div>
          <div class="w-[130px]">
            <div class="font-semibold">{s.messages.toLocaleString()} emails</div>
            <div class="text-[12.5px] text-muted">30 days</div>
          </div>
          <label class="sr-only" for="sd-{s.type}-{s.value}">What happens to mail from {nameOf(s)}</label>
          <select id="sd-{s.type}-{s.value}" class="field flex-[1_1_200px] px-2.5" value={routingOf(s)} onchange={(e) => route(s, e.currentTarget)}>
            <option value="auto">Let my rules decide</option>
            <option value="keep">Always keep in Inbox</option>
            {#each rules.list as r (r.id)}
              <option value={String(r.id)}>Always: {r.name}</option>
            {/each}
            <option value="trash">Always trash</option>
          </select>
          {#if features.unsubscribe}
            <!-- P2: the Unsubscribe button goes here once the contract has a route for it -->
          {/if}
        </div>
      {:else}
        <p class="px-[18px] py-3.5 text-[13px] text-muted">No senders found.</p>
      {/each}
      {#if senders.next}
        <div class="border-t border-line-divider px-[18px] py-3">
          <button type="button" class="btn px-3" onclick={more}>Show more</button>
        </div>
      {/if}
    </section>

    <section aria-label="Learned sender rules" class="card flex flex-col gap-2.5 px-[18px] py-4">
      <h2 class="text-[15px]">Learned from your corrections and consistent decisions</h2>
      {#each senders.learned as r (r.type + r.value)}
        <div class="flex flex-wrap items-center justify-between gap-2 text-[13.5px]">
          <span><span class="font-mono">{r.value}</span> → {targetOf(r)}</span>
          <button type="button" class="btn min-h-9 px-3" aria-label="Forget {r.value}" onclick={() => forget(r)}>Forget</button>
        </div>
      {:else}
        <p class="text-[13px] text-muted">Nothing learned yet.</p>
      {/each}
    </section>
  {/if}
</div>
