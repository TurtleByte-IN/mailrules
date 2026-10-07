<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { Account } from '../lib/api/accounts';
  import { clock, money } from '../lib/format';
  import { accounts, reconnect, setPaused, statuses, test } from '../lib/state/accounts.svelte';
  import { kind, outcome } from '../lib/state/activity.svelte';
  import { callsLine, health, healthLine, load, overview, split } from '../lib/state/overview.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import LoadError from './activity/LoadError.svelte';

  load();

  const stats = $derived(overview.stats);
  const top = $derived(healthLine(accounts.list));
  const parts = $derived(stats ? split(stats.went) : []);
  const most = $derived(Math.max(1, ...(stats?.top_rules ?? []).map((r) => r.hits)));
  const quiet = $derived(stats?.quiet_rules ?? 0);
  const mailboxes = $derived(accounts.list.length + (accounts.list.length === 1 ? ' mailbox' : ' mailboxes'));

  const tiles = $derived(
    stats
      ? [
          { label: 'Processed today', value: stats.counts.processed, sub: 'across ' + mailboxes, href: '/activity' },
          { label: 'Sorted', value: stats.went.sorted, sub: 'moved, archived or flagged', href: '/activity?outcome=sorted' },
          { label: 'Trashed', value: stats.went.trashed, sub: 'restorable for 30 days', href: '/activity?outcome=trashed' },
          { label: 'Needs review', value: stats.counts.review, sub: stats.counts.review ? 'Review now' : 'All clear', href: '/review', warn: stats.counts.review > 0 },
          { label: 'Cost today', value: money(stats.cost_usd), sub: callsLine(stats.calls_by_model), href: '/usage' },
        ]
      : [],
  );

  const chips = { ok: '', trash: 'chip-trash', none: 'chip-neutral', review: 'chip-review' };
  const act = (a: Account, action: 'reconnect' | 'resume') => (action === 'resume' ? setPaused(a.id, false) : reconnect(a.id));

  // A test opens its own connection and leaves the mailbox's watcher alone.
  let testing = $state<Record<number, boolean>>({});
  async function runTest(id: number) {
    testing[id] = true;
    await test(id);
    testing[id] = false;
  }
</script>

<div class="flex flex-col gap-[22px]">
  <header class="flex flex-wrap items-end justify-between gap-2.5">
    <div>
      <h1>Overview</h1>
      <p class="mt-1 text-secondary">Is MailRules working? Today's numbers, mailbox health and the rules doing the work.</p>
    </div>
    {#if accounts.loaded}
      <span class="inline-flex h-9 items-center gap-1.5 rounded bg-selected px-3 text-[13px] font-medium">
        <span class="size-2 rounded-sm {top.ok ? 'bg-live' : 'bg-attention'}"></span>{top.text}
      </span>
    {/if}
  </header>

  {#if overview.error}
    <LoadError message={overview.error} retry={load} />
  {/if}

  {#if stats}
    <section aria-label="Today" class="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-3">
      {#each tiles as t (t.label)}
        <a href={t.href} use:link class="rounded-md border px-[18px] py-4 no-underline {t.warn ? 'border-warn-line bg-warn-bg text-warn' : 'border-line-card text-ink'}">
          <div class="text-[12.5px] font-medium {t.warn ? 'text-review' : 'text-secondary'}">{t.label}</div>
          <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{t.value}</div>
          <div class="text-[12.5px] {t.warn ? 'text-review' : 'text-secondary'}">{t.sub}</div>
        </a>
      {/each}
    </section>

    <section aria-label="Where today's mail went" class="card flex flex-col gap-3 p-[18px]">
      <div class="flex flex-wrap items-baseline justify-between gap-1.5">
        <h2 class="text-[15px]">Where today's mail went</h2>
        <span class="text-[12.5px] text-muted">{stats.counts.processed} processed</span>
      </div>
      <div class="flex h-3 overflow-hidden rounded-sm bg-line-divider" aria-hidden="true">
        {#each parts as p (p.label)}
          <div class="h-3 {p.fill}" style:width="{p.pct}%"></div>
        {/each}
      </div>
      <div class="flex flex-wrap gap-x-3.5 gap-y-1 text-[12.5px] text-secondary">
        {#each parts as p (p.label)}
          <a href="/activity?outcome={p.outcome}" use:link class="inline-flex items-center gap-[5px] text-secondary no-underline max-md:min-h-11"><span class="size-2.5 rounded-sm {p.fill}"></span>{p.label} {p.n}</a>
        {/each}
      </div>
    </section>
  {/if}

  <div class="flex flex-wrap items-start gap-3">
    <section aria-label="Mailbox health" class="card min-w-0 flex-[3_1_460px] overflow-x-auto">
      <div class="flex items-center justify-between px-[18px] py-4">
        <h2 class="text-[15px]">Mailbox health</h2>
        <a href="/accounts" use:link class="btn min-h-9 px-3 text-[13px] no-underline">Manage</a>
      </div>
      {#each accounts.list as a (a.id)}
        {@const h = health(a)}
        <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-line-divider px-[18px] py-3">
          <span class="min-w-0 flex-[1_1_180px]">
            <span class="font-medium">{a.label}</span><br />
            <span class="text-[12.5px] text-muted">{a.username} · {a.folder_count} folders</span>
          </span>
          <span class="chip h-[22px] text-[12.5px] {h.chip}">{statuses[a.status].label}</span>
          <span class="min-w-0 flex-[1_1_200px] text-[12.5px] text-secondary">{h.detail}</span>
          {#if h.action === 'go'}
            <a href="/accounts" use:link class="btn min-h-9 px-3 text-[13px] no-underline">{h.label}</a>
          {:else if h.action === 'wait'}
            <button type="button" class="btn min-h-9 px-3 text-[13px]" disabled>{h.label}</button>
          {:else if h.action === 'test'}
            <button type="button" class="btn min-h-9 px-3 text-[13px]" aria-label="Test {a.label}" disabled={testing[a.id]} onclick={() => runTest(a.id)}>{testing[a.id] ? 'Testing…' : h.label}</button>
          {:else if h.action === 'reconnect' || h.action === 'resume'}
            {@const action = h.action}
            <button type="button" class="btn min-h-9 px-3 text-[13px]" onclick={() => act(a, action)}>{h.label}</button>
          {/if}
        </div>
      {:else}
        {#if accounts.loaded}
          <div class="flex flex-wrap items-center justify-between gap-2 border-t border-line-divider px-[18px] py-3 text-secondary">
            No mailbox connected yet.
            <a href="/accounts" use:link class="btn-primary no-underline">Add mailbox</a>
          </div>
        {/if}
      {/each}
    </section>

    {#if stats}
      <section aria-label="Top rules today" class="card flex min-w-0 flex-[2_1_300px] flex-col gap-3 p-[18px]">
        <div class="flex items-baseline justify-between">
          <h2 class="text-[15px]">Top rules today</h2>
          <span class="text-[12.5px] text-muted">by matches</span>
        </div>
        {#each stats.top_rules as r (r.rule_id)}
          <a href={r.rule_id === null ? '/rules' : '/rules?id=' + r.rule_id} use:link class="flex items-center gap-3 text-ink no-underline max-md:min-h-11">
            <span class="flex-[0_0_110px] truncate font-medium">{r.rule_name}</span>
            <span class="h-2 flex-auto overflow-hidden rounded-sm bg-line-divider"><span class="block h-2 bg-ink" style:width="{Math.max(3, (r.hits / most) * 100)}%"></span></span>
            <span class="flex-[0_0_32px] text-right font-mono text-[12.5px]">{r.hits}</span>
          </a>
        {:else}
          <div class="text-[12.5px] text-secondary">No rule has matched today.</div>
        {/each}
        {#if rules.loaded && rules.list.length}
          <div class="text-[12.5px] text-secondary">
            {quiet ? quiet + (quiet === 1 ? ' rule has' : ' rules have') + ' not matched anything today.' : 'Every active rule matched at least once today.'}
          </div>
        {/if}
      </section>
    {/if}
  </div>

  {#if stats}
    <section aria-label="Latest decisions" class="card overflow-hidden">
      <div class="flex items-center justify-between px-[18px] py-4">
        <h2 class="text-[15px]">Latest decisions</h2>
        <a href="/activity" use:link class="btn min-h-9 px-3 text-[13px] no-underline">Open activity</a>
      </div>
      {#each overview.latest as row (row.id)}
        <a href="/activity" use:link class="flex min-h-11 items-center gap-2.5 border-t border-line-divider px-[18px] text-[13px] text-ink no-underline max-md:flex-wrap max-md:gap-y-0.5 max-md:py-2.5">
          <span class="flex-[0_0_62px] font-mono text-xs text-muted">{clock(row.created_at)}</span>
          <span class="min-w-0 flex-[0_0_130px] truncate font-medium max-md:flex-1">{row.from}</span>
          <span class="min-w-0 flex-[1_1_160px] truncate text-nav max-md:order-last max-md:basis-full">{row.subject}</span>
          <span class="chip {chips[kind(row)]}">{outcome(row)}</span>
        </a>
      {:else}
        <div class="border-t border-line-divider px-[18px] py-3 text-secondary">Nothing sorted yet. New mail shows up here as it arrives.</div>
      {/each}
    </section>
  {/if}
</div>
