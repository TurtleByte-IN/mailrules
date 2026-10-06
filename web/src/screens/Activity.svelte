<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { ActivityItem } from '../lib/api/activity';
  import { clock, confidence, day, money } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { activity, canUndo, kind, load, loadMore, loadStats, open, outcome, ruleName, undo, undoLastHour, undone, type Kind } from '../lib/state/activity.svelte';
  import { load as loadReview, review } from '../lib/state/review.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import Detail from './activity/Detail.svelte';
  import LoadError from './activity/LoadError.svelte';

  load();
  loadStats();
  loadReview();

  const kinds: [Kind, string][] = [['ok', 'Sorted'], ['trash', 'Trashed'], ['none', 'No rule'], ['review', 'Needs review']];
  const today = Math.floor(Date.now() / 1000);

  let selected = $state<number | null>(null);
  const rows = $derived(activity.list);
  const filtered = $derived(Boolean(activity.filter.rule || activity.filter.account || activity.filter.kind));
  // Wide screens always show one decision beside the feed, the first row until one is picked.
  // Narrow screens open it under the row that was tapped.
  const shownId = $derived((rows.find((r) => r.id === selected) ?? rows[0])?.id);
  const detail = $derived(activity.detail?.id === shownId ? activity.detail : null);
  const stats = $derived(activity.stats);
  // The daemon lists a model once per purpose (decide, escalate, compose, test); the tile shows one figure per model.
  const calls = $derived.by(() => {
    const by = new Map<string, number>();
    for (const m of stats?.calls_by_model ?? []) by.set(m.model, (by.get(m.model) ?? 0) + m.calls);
    return [...by].map(([model, n]) => `${model} ${n} ${n === 1 ? 'call' : 'calls'}`).join(' · ');
  });

  $effect(() => {
    if (shownId) open(shownId);
  });

  // The feed runs past today: older rows show their date instead of the time.
  const when = (r: ActivityItem) => {
    const ts = r.received_at ?? r.created_at;
    return day(ts) === day(today) ? clock(ts) : day(ts);
  };

  const mailboxes = $derived(accounts.list.length + (accounts.list.length === 1 ? ' mailbox' : ' mailboxes'));

  const stage = (r: ActivityItem) => {
    const d = r.decision;
    if (r.correction) return 'you · corrected';
    if (!d) return '';
    if (d.stage === 'sender') return 'sender · learned';
    if (d.stage === 'condition') return 'condition · free';
    // No model was asked (none is set): there is no confidence to show.
    if (!d.model) return '';
    return [d.model, (d.stage === 'none' ? 'none ' : '') + confidence(d.confidence)].filter(Boolean).join(' · ');
  };

  const chip = (r: ActivityItem) => {
    const k = kind(r);
    return undone(r) || k === 'none' ? 'chip-neutral' : k === 'trash' ? 'chip-trash' : k === 'review' ? 'chip-review' : '';
  };
</script>

<div class="flex flex-col gap-[22px]">
  <header class="flex flex-wrap items-end justify-between gap-4">
    <div>
      <h1>Activity</h1>
      <p class="mt-1 text-secondary">Every decision, live. Click a row to see why; undo anything for 30 days.</p>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      <button type="button" class="btn min-h-9 px-3 text-[13px]" onclick={undoLastHour}>Undo the last hour</button>
      <span class="inline-flex h-9 items-center gap-2 rounded bg-selected px-3 text-[13px] font-semibold">
        <span class="size-2 rounded-sm bg-live"></span>Live on {mailboxes}
      </span>
    </div>
  </header>

  {#if activity.statsError}<LoadError message={activity.statsError} retry={loadStats} />{/if}
  <section aria-label="Today at a glance" class="grid grid-cols-[repeat(auto-fit,minmax(190px,1fr))] gap-3">
    {#if stats}
      <div class="card px-[18px] py-4">
        <div class="text-xs font-medium text-secondary">Sorted today</div>
        <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{stats.counts.sorted}</div>
        <div class="text-xs text-muted">across {mailboxes}</div>
      </div>
    {/if}
    <a href="/review" use:link class={['block rounded-md border border-warn-line bg-warn-bg px-[18px] py-4 text-warn no-underline', !stats && 'sm:max-w-64']}>
      <div class="text-xs font-medium text-review">Needs review</div>
      <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{review.total}</div>
      <div class="text-xs text-review">Still in your inbox · Review now</div>
    </a>
    {#if stats}
      <div class="card px-[18px] py-4">
        <div class="text-xs font-medium text-secondary">Decided without a model</div>
        <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{Math.round(stats.decided_without_model * 100)}%</div>
        <div class="text-xs text-muted">sender rules and conditions</div>
      </div>
      <div class="card px-[18px] py-4">
        <div class="text-xs font-medium text-secondary">Model cost today</div>
        <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{money(stats.cost_usd)}</div>
        <div class="text-xs text-muted">{calls}</div>
      </div>
    {/if}
  </section>

  <div class="grid items-start gap-[18px] xl:grid-cols-[2fr_1fr]">
    <section aria-label="Activity feed" class="card min-w-0 overflow-hidden">
      <div class="flex items-center justify-between px-[18px] py-3.5">
        <h2 class="text-[15px]">Today, {new Date().toLocaleDateString([], { weekday: 'short' })} {day(today)}</h2>
        <span class="text-xs text-muted">Latest first</span>
      </div>
      <div class="flex flex-wrap gap-3 px-[18px] pb-3.5">
        <label class="flex min-w-0 flex-[1_1_140px] flex-col gap-1.5">
          <span class="label">Rule</span>
          <select class="field" bind:value={activity.filter.rule} onchange={load}>
            <option value="">All rules</option>
            {#each rules.list as r (r.id)}
              <option value={String(r.id)}>{r.name}</option>
            {/each}
          </select>
        </label>
        <label class="flex min-w-0 flex-[1_1_140px] flex-col gap-1.5">
          <span class="label">Mailbox</span>
          <select class="field" bind:value={activity.filter.account} onchange={load}>
            <option value="">All mailboxes</option>
            {#each accounts.list as a (a.id)}
              <option value={String(a.id)}>{a.label}</option>
            {/each}
          </select>
        </label>
        <label class="flex min-w-0 flex-[1_1_140px] flex-col gap-1.5">
          <span class="label">Outcome</span>
          <select class="field" bind:value={activity.filter.kind} onchange={load}>
            <option value="">All outcomes</option>
            {#each kinds as [value, name] (value)}
              <option {value}>{name}</option>
            {/each}
          </select>
        </label>
      </div>
      {#if activity.error}
        <div class="border-t border-line-divider p-[18px]"><LoadError message={activity.error} retry={load} /></div>
      {/if}
      <ul>
        {#each rows as row (row.id)}
          {@const on = row.id === shownId}
          {@const inReview = row.state === 'review'}
          <li class="border-t border-l-[3px] border-t-line-divider {on ? 'border-l-signal bg-selected-row' : 'border-l-transparent'}">
            <div class="flex flex-wrap gap-x-4 gap-y-2.5 py-3.5 pr-[18px] pl-[15px]">
              <div class="w-11 shrink-0 pt-0.5 font-mono text-xs text-muted">{when(row)}</div>
              <button
                type="button"
                aria-current={on ? 'true' : undefined}
                class="min-w-0 flex-[1_1_200px] border-0 bg-transparent p-0 text-left text-inherit"
                onclick={() => (selected = row.id)}
              >
                <div class="break-words"><span class="font-semibold">{row.from_name || row.from}</span><span class="text-nav"> · {row.subject}</span></div>
                <div class="mt-0.5 text-xs text-muted">{row.decision?.reason}</div>
                <div class="mt-2 flex flex-wrap gap-1.5">
                  <span class="chip {chip(row)}">{ruleName(row)}</span>
                  {#if stage(row)}<span class="chip chip-neutral font-mono font-normal">{stage(row)}</span>{/if}
                </div>
              </button>
              <div class="flex flex-wrap items-center gap-2">
                <span class="mr-1 text-[13px]">{outcome(row)}</span>
                {#if inReview}
                  <a href="/review" use:link class="btn-primary min-h-9 px-3.5 text-[13px] no-underline">Review</a>
                {/if}
                {#if canUndo(row)}
                  <button type="button" class="btn min-h-9 gap-1.5 px-3 text-[13px]" onclick={() => undo(row)}>
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <path d="M9 14 4 9l5-5" /><path d="M4 9h11a5 5 0 0 1 0 10h-3" />
                    </svg>Undo
                  </button>
                {/if}
                {#if !inReview && !undone(row)}
                  <button type="button" class="btn min-h-9 px-3 text-[13px]" onclick={() => (selected = row.id)}>Wrong?</button>
                {/if}
              </div>
            </div>
            {#if selected === row.id && (detail || activity.detailError)}
              <div role="group" aria-label="Decision details" class="flex flex-col gap-4 border-t border-line-divider bg-surface p-[18px] xl:hidden">
                {@render panel('inline')}
              </div>
            {/if}
          </li>
        {:else}
          {#if activity.loaded && !activity.error}
            <li class="border-t border-line-divider px-[18px] py-10 text-center text-secondary">
              {filtered ? 'No activity matches these filters.' : 'Nothing sorted yet. New mail shows up here as it arrives.'}
            </li>
          {/if}
        {/each}
      </ul>
      {#if activity.next}
        <div class="border-t border-line-divider p-3.5 text-center">
          <button type="button" class="btn" onclick={loadMore}>Load more</button>
        </div>
      {/if}
    </section>

    {#if detail || activity.detailError}
      <aside aria-label="Decision details" class="card hidden min-w-0 flex-col gap-4 p-[18px] xl:flex">
        {@render panel('side')}
      </aside>
    {/if}
  </div>
</div>

{#snippet panel(id: string)}
  {#if detail}
    {#key detail.id}<Detail message={detail} {id} />{/key}
  {:else}
    <LoadError message={activity.detailError} retry={() => shownId && open(shownId)} />
  {/if}
{/snippet}
