<script lang="ts">
  import { features } from '../lib/features';
  import { domainOf, type SenderRule, type Sort } from '../lib/api/senders';
  import { rules } from '../lib/state/rules.svelte';
  import { forget, load, routingOf, search, senders, setRouting, setSort, targetOf, unsubscribe } from '../lib/state/senders.svelte';

  load();

  let query = $state('');
  const rows = $derived(search(senders.list, query));
  // The prototype lists what the user set first, then what was learned.
  const learned = $derived([...senders.rules].sort((a, b) => Number(a.source === 'learned') - Number(b.source === 'learned')));

  const why = (r: SenderRule) =>
    r.why ?? (senders.list.some((s) => s.unsubscribed && domainOf(s.address) === r.value) ? 'after unsubscribing' : 'set by you');
</script>

<div class="flex flex-col gap-[18px]">
  <header>
    <h1>Senders</h1>
    <p class="mt-1 text-secondary">Who emails you most. Set a sender once and their mail never needs a model again.</p>
  </header>

  <div class="flex flex-wrap gap-2">
    <label class="field flex max-w-[420px] flex-[1_1_260px] items-center gap-2 text-muted focus-within:outline-2 focus-within:outline-offset-2 focus-within:outline-signal">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
        <circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" />
      </svg>
      <span class="sr-only">Search senders</span>
      <input type="search" class="w-full border-0 bg-transparent text-ink outline-none" placeholder="Search senders" bind:value={query} />
    </label>
    <label class="sr-only" for="sd-sort">Sort</label>
    <select id="sd-sort" class="field px-2.5" value={senders.sort} onchange={(e) => setSort(e.currentTarget.value as Sort)}>
      <option value="volume">Most email first</option>
      <option value="unread">Least opened first</option>
    </select>
  </div>

  <section aria-label="Sender list" class="card overflow-hidden">
    {#each rows as s, i (s.address)}
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2.5 px-[18px] py-3.5 {i ? 'border-t border-line-divider' : ''} {s.unsubscribed ? 'opacity-60' : ''}">
        <div class="min-w-0 flex-[1_1_220px]">
          <div class="font-semibold">{s.name}</div>
          <div class="font-mono text-[12.5px] text-muted">{s.address}</div>
        </div>
        <div class="w-[130px]">
          <div class="font-semibold">{s.count} emails</div>
          <div class="text-[12.5px] text-muted">{s.readPct}% opened · 30 days</div>
        </div>
        <label class="sr-only" for="sd-{s.address}">What happens to mail from {s.name}</label>
        <select id="sd-{s.address}" class="field flex-[1_1_200px] px-2.5" value={routingOf(s)} onchange={(e) => setRouting(s, e.currentTarget.value)}>
          <option value="auto">Let my rules decide</option>
          <option value="keep">Always keep in Inbox</option>
          {#each rules.list as r (r.id)}
            <option value={r.id}>Always: {r.name}</option>
          {/each}
          <option value="trash">Always trash</option>
        </select>
        {#if !features.unsubscribe}
          <!-- P2: hidden until the backend can act on List-Unsubscribe -->
        {:else if s.list}
          <button type="button" class="btn px-3" disabled={s.unsubscribed} onclick={() => unsubscribe(s)}>
            {s.unsubscribed ? 'Unsubscribed' : 'Unsubscribe'}
          </button>
        {:else}
          <span class="w-[120px] text-[12.5px] text-muted">No unsubscribe link</span>
        {/if}
      </div>
    {/each}
  </section>

  <section aria-label="Learned sender rules" class="card flex flex-col gap-2.5 px-[18px] py-4">
    <h2 class="text-[15px]">Learned from your corrections and consistent decisions</h2>
    {#each learned as r (r.type + r.value)}
      <div class="flex flex-wrap items-center justify-between gap-2 text-[13.5px]">
        <span><span class="font-mono">{r.value}</span> → {targetOf(r)} <span class="text-muted">· {why(r)}</span></span>
        <button type="button" class="btn min-h-9 px-3" aria-label="Forget {r.value}" onclick={() => forget(r)}>Forget</button>
      </div>
    {:else}
      <p class="text-[13px] text-muted">Nothing learned yet.</p>
    {/each}
  </section>
</div>
