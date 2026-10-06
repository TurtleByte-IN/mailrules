<script lang="ts">
  import { link } from 'svelte-spa-router';
  import type { ActivityRow, Kind } from '../lib/api/activity';
  import { clock, confidence, day } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { activity, canUndo, load, matches, open, undo, undoLastHour } from '../lib/state/activity.svelte';
  import { load as loadReview, review } from '../lib/state/review.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import Detail from './activity/Detail.svelte';

  load();
  loadReview();

  const kinds: [Kind, string][] = [['ok', 'Sorted'], ['trash', 'Trashed'], ['none', 'No rule'], ['review', 'Needs review']];
  const today = Math.floor(Date.now() / 1000);

  let selected = $state<string | null>(null);
  const rows = $derived(activity.list.filter((r) => matches(r, activity.filter)));
  // Wide screens always show one decision beside the feed, the first row until one is picked.
  // Narrow screens open it under the row that was tapped.
  const shownId = $derived((rows.find((r) => r.id === selected) ?? rows[0])?.id);
  const detail = $derived(activity.detail?.id === shownId ? activity.detail : null);

  $effect(() => {
    if (shownId) open(shownId);
  });

  const stage = (r: ActivityRow) =>
    r.stage === 'sender'
      ? 'sender · learned'
      : r.stage === 'condition'
        ? 'condition · free'
        : r.stage === 'corrected' || r.stage === 'reviewed'
          ? 'you · ' + r.stage
          : `${r.model} · ${r.stage === 'none' ? 'none ' : ''}${confidence(r.confidence ?? 0)}`;

  const chip = (r: ActivityRow) =>
    r.undone || r.kind === 'none' ? 'chip-neutral' : r.kind === 'trash' ? 'chip-trash' : r.kind === 'review' ? 'chip-review' : '';
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
        <span class="size-2 rounded-sm bg-live"></span>Live on {accounts.list.length} mailboxes
      </span>
    </div>
  </header>

  <section aria-label="Today at a glance">
    <a href="/review" use:link class="block rounded-md border border-warn-line bg-warn-bg px-[18px] py-4 text-warn no-underline sm:max-w-64">
      <div class="text-xs font-medium text-review">Needs review</div>
      <div class="mt-1 text-[28px] font-semibold tracking-[-0.02em]">{review.list.length}</div>
      <div class="text-xs text-review">Still in your inbox · Review now</div>
    </a>
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
          <select class="field" bind:value={activity.filter.rule}>
            <option value="">All rules</option>
            {#each rules.list as r (r.id)}
              <option value={r.id}>{r.name}</option>
            {/each}
          </select>
        </label>
        <label class="flex min-w-0 flex-[1_1_140px] flex-col gap-1.5">
          <span class="label">Mailbox</span>
          <select class="field" bind:value={activity.filter.account}>
            <option value="">All mailboxes</option>
            {#each accounts.list as a (a.id)}
              <option value={a.id}>{a.email}</option>
            {/each}
          </select>
        </label>
        <label class="flex min-w-0 flex-[1_1_140px] flex-col gap-1.5">
          <span class="label">Outcome</span>
          <select class="field" bind:value={activity.filter.kind}>
            <option value="">All outcomes</option>
            {#each kinds as [value, name] (value)}
              <option {value}>{name}</option>
            {/each}
          </select>
        </label>
      </div>
      <ul>
        {#each rows as row (row.id)}
          {@const on = row.id === shownId}
          <li class="border-t border-l-[3px] border-t-line-divider {on ? 'border-l-signal bg-selected-row' : 'border-l-transparent'}">
            <div class="flex flex-wrap gap-x-4 gap-y-2.5 py-3.5 pr-[18px] pl-[15px]">
              <div class="w-11 shrink-0 pt-0.5 font-mono text-xs text-muted">{clock(row.receivedAt)}</div>
              <button
                type="button"
                aria-current={on ? 'true' : undefined}
                class="min-w-0 flex-[1_1_200px] border-0 bg-transparent p-0 text-left text-inherit"
                onclick={() => (selected = row.id)}
              >
                <div class="break-words"><span class="font-semibold">{row.sender}</span><span class="text-nav"> · {row.subject}</span></div>
                <div class="mt-0.5 text-xs text-muted">{row.reason}</div>
                <div class="mt-2 flex flex-wrap gap-1.5">
                  <span class="chip {chip(row)}">{row.rule}</span>
                  <span class="chip chip-neutral font-mono font-normal">{stage(row)}</span>
                </div>
              </button>
              <div class="flex flex-wrap items-center gap-2">
                <span class="mr-1 text-[13px]">{row.undone ? 'Undone · back in Inbox' : row.outcome}</span>
                {#if row.kind === 'review'}
                  <a href="/review" use:link class="btn-primary min-h-9 px-3.5 text-[13px] no-underline">Review</a>
                {/if}
                {#if canUndo(row)}
                  <button type="button" class="btn min-h-9 gap-1.5 px-3 text-[13px]" onclick={() => undo(row)}>
                    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                      <path d="M9 14 4 9l5-5" /><path d="M4 9h11a5 5 0 0 1 0 10h-3" />
                    </svg>Undo
                  </button>
                {/if}
                {#if row.kind !== 'review' && !row.undone}
                  <button type="button" class="btn min-h-9 px-3 text-[13px]" onclick={() => (selected = row.id)}>Wrong?</button>
                {/if}
              </div>
            </div>
            {#if detail && selected === row.id}
              <div role="group" aria-label="Decision details" class="flex flex-col gap-4 border-t border-line-divider bg-surface p-[18px] xl:hidden">
                {#key detail.id}<Detail message={detail} id="inline" />{/key}
              </div>
            {/if}
          </li>
        {:else}
          <li class="border-t border-line-divider px-[18px] py-10 text-center text-secondary">No activity matches these filters.</li>
        {/each}
      </ul>
    </section>

    {#if detail}
      <aside aria-label="Decision details" class="card hidden min-w-0 flex-col gap-4 p-[18px] xl:flex">
        {#key detail.id}<Detail message={detail} id="side" />{/key}
      </aside>
    {/if}
  </div>
</div>
