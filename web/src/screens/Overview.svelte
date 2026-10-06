<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { Account } from '../lib/api/accounts';
  import { clock, money } from '../lib/format';
  import { accounts, reconnect, setPaused, statuses } from '../lib/state/accounts.svelte';
  import { kind, outcome } from '../lib/state/activity.svelte';
  import { callsLine, health, healthLine, load, overview, split } from '../lib/state/overview.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import LoadError from './activity/LoadError.svelte';

  load();

  const stats = $derived(overview.stats);
  const top = $derived(healthLine(accounts.list));
  const parts = $derived(stats ? split(stats.counts) : []);
  const most = $derived(Math.max(1, ...(stats?.top_rules ?? []).map((r) => r.hits)));
  const quiet = $derived(rules.list.filter((r) => r.enabled && r.last_match_at === null).length);
  const mailboxes = $derived(accounts.list.length + (accounts.list.length === 1 ? ' mailbox' : ' mailboxes'));

  const tiles = $derived(
    stats
      ? [
          { label: 'Processed today', value: stats.counts.processed, sub: 'across ' + mailboxes, href: '/activity' },
          { label: 'Sorted', value: parts[0].n, sub: 'moved, archived or flagged', href: '/activity' },
          { label: 'Trashed', value: stats.counts.trashed, sub: 'restorable for 30 days', href: '/activity' },
          { label: 'Needs review', value: stats.counts.review, sub: stats.counts.review ? 'Review now' : 'All clear', href: '/review', warn: stats.counts.review > 0 },
          { label: 'Cost today', value: money(stats.cost_usd), sub: callsLine(stats.calls_by_model), href: '/usage' },
        ]
      : [],
  );

  const chips = { ok: '', trash: 'chip-trash', none: 'chip-neutral', review: 'chip-review' };
  const act = (a: Account, action: 'reconnect' | 'resume') => (action === 'resume' ? setPaused(a.id, false) : reconnect(a.id));
</script>

<div class="flex flex-col gap-3.5">
  <header class="flex flex-wrap items-end justify-between gap-2.5">
    <div>
      <h1>Overview</h1>
      <p class="mt-1 text-secondary">Is MailRules working? Today's numbers, mailbox health and the rules doing the work.</p>
    </div>
    {#if accounts.loaded}
      <span class="inline-flex h-[26px] items-center gap-1.5 rounded bg-selected px-[9px] text-xs font-medium">
        <span class="size-2 rounded-sm {top.ok ? 'bg-live' : 'bg-attention'}"></span>{top.text}
      </span>
    {/if}
  </header>

  {#if overview.error}
    <LoadError message={overview.error} retry={load} />
  {/if}

  {#if stats}
    <section aria-label="Today" class="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-2">
      {#each tiles as t (t.label)}
        <a href={t.href} use:link class="rounded-md border px-3.5 py-2.5 no-underline {t.warn ? 'border-warn-line bg-warn-bg text-warn' : 'border-line-card text-ink'}">
          <div class="text-[11.5px] font-medium {t.warn ? 'text-review' : 'text-secondary'}">{t.label}</div>
          <div class="mt-1 text-[17px] font-medium tracking-[-0.02em]">{t.value}</div>
          <div class="text-[11.5px] {t.warn ? 'text-review' : 'text-secondary'}">{t.sub}</div>
        </a>
      {/each}
    </section>

    <section aria-label="Where today's mail went" class="card flex flex-col gap-2 p-3">
      <div class="flex flex-wrap items-baseline justify-between gap-1.5">
        <h2 class="text-[13px]">Where today's mail went</h2>
        <span class="text-[11.5px] text-muted">{stats.counts.processed} processed</span>
      </div>
      <div class="flex h-3 overflow-hidden rounded-sm bg-line-divider" aria-hidden="true">
        {#each parts as p (p.label)}
          <div class="h-3 {p.fill}" style:width="{p.pct}%"></div>
        {/each}
      </div>
      <div class="flex flex-wrap gap-x-3.5 gap-y-1 text-[11.5px] text-secondary">
        {#each parts as p (p.label)}
          <span class="inline-flex items-center gap-[5px]"><span class="size-2.5 rounded-sm {p.fill}"></span>{p.label} {p.n}</span>
        {/each}
      </div>
    </section>
  {/if}

  <div class="flex flex-wrap items-start gap-3">
    <section aria-label="Mailbox health" class="card min-w-0 flex-[3_1_460px] overflow-x-auto">
      <div class="flex items-center justify-between px-3.5 py-2.5">
        <h2 class="text-[13px]">Mailbox health</h2>
        <a href="/accounts" use:link class="btn min-h-[26px] px-[9px] text-xs no-underline">Manage</a>
      </div>
      {#each accounts.list as a (a.id)}
        {@const h = health(a)}
        <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5 border-t border-line-divider px-3.5 py-2 text-[12.5px]">
          <span class="min-w-0 flex-[1_1_180px]">
            <span class="font-medium">{a.label}</span><br />
            <span class="text-[11.5px] text-muted">{a.username} · {a.folder_count} folders</span>
          </span>
          <span class="chip h-[22px] gap-[5px] text-[11.5px] {h.chip}">{statuses[a.status].label}</span>
          <span class="min-w-0 flex-[1_1_200px] text-[11.5px] text-secondary">{h.detail}</span>
          {#if h.action === 'go'}
            <a href="/accounts" use:link class="btn min-h-[26px] px-[9px] text-xs no-underline">{h.label}</a>
          {:else if h.action === 'wait'}
            <button type="button" class="btn min-h-[26px] px-[9px] text-xs" disabled>{h.label}</button>
          {:else if h.action === 'reconnect' || h.action === 'resume'}
            {@const action = h.action}
            <button type="button" class="btn min-h-[26px] px-[9px] text-xs" onclick={() => act(a, action)}>{h.label}</button>
          {/if}
        </div>
      {:else}
        {#if accounts.loaded}
          <div class="flex flex-wrap items-center justify-between gap-2 border-t border-line-divider px-3.5 py-2.5 text-secondary">
            No mailbox connected yet.
            <a href="/accounts" use:link class="btn-primary no-underline">Add mailbox</a>
          </div>
        {/if}
      {/each}
    </section>

    {#if stats}
      <section aria-label="Top rules today" class="card flex min-w-0 flex-[2_1_300px] flex-col gap-2 p-3">
        <div class="flex items-baseline justify-between">
          <h2 class="text-[13px]">Top rules today</h2>
          <span class="text-[11.5px] text-muted">by matches</span>
        </div>
        {#each stats.top_rules as r (r.rule_id)}
          <a href={r.rule_id === null ? '/rules' : '/rules?id=' + r.rule_id} use:link class="flex items-center gap-2.5 text-xs text-ink no-underline">
            <span class="flex-[0_0_130px] truncate font-medium">{r.rule_name}</span>
            <span class="h-2 flex-auto overflow-hidden rounded-sm bg-line-divider"><span class="block h-2 bg-ink" style:width="{Math.max(3, (r.hits / most) * 100)}%"></span></span>
            <span class="flex-[0_0_32px] text-right font-mono text-[11.5px]">{r.hits}</span>
          </a>
        {:else}
          <div class="text-[11.5px] text-secondary">No rule has matched today.</div>
        {/each}
        {#if rules.loaded && rules.list.length}
          <div class="text-[11.5px] text-muted">
            {quiet ? quiet + (quiet === 1 ? ' rule has' : ' rules have') + ' not matched anything yet.' : 'Every active rule has matched at least once.'}
          </div>
        {/if}
      </section>
    {/if}
  </div>

  {#if stats}
    <section aria-label="Latest decisions" class="card overflow-hidden">
      <div class="flex items-center justify-between px-3.5 py-2.5">
        <h2 class="text-[13px]">Latest decisions</h2>
        <a href="/activity" use:link class="btn min-h-[26px] px-[9px] text-xs no-underline">Open activity</a>
      </div>
      {#each overview.latest as row (row.id)}
        <a href="/activity" use:link class="flex min-h-8 items-center gap-2.5 border-t border-line-divider px-3.5 text-xs text-ink no-underline">
          <span class="flex-[0_0_38px] font-mono text-[11px] text-muted">{clock(row.created_at)}</span>
          <span class="flex-[0_0_110px] truncate font-medium">{row.from}</span>
          <span class="min-w-0 flex-[1_1_160px] truncate text-nav">{row.subject}</span>
          <span class="chip {chips[kind(row)]}">{outcome(row)}</span>
        </a>
      {:else}
        <div class="border-t border-line-divider px-3.5 py-2.5 text-secondary">Nothing sorted yet. New mail shows up here as it arrives.</div>
      {/each}
    </section>
  {/if}
</div>
