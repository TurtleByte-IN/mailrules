<script lang="ts">
  import type { Batch, Preview } from '../lib/api/cleanup';
  import Waiting from '../lib/components/Waiting.svelte';
  import { clock, day, money } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { cleanup, load, more, noUndo, outcome, preview, run, setScope, undo, UNDO_DAYS, type Scope } from '../lib/state/cleanup.svelte';
  import { settings } from '../lib/state/settings.svelte';

  load();

  type Group = Preview['groups'][number];

  // The mailbox list arrives after the shell mounts; start on the first one.
  $effect(() => {
    if (!cleanup.scope.accountId && accounts.list.length) setScope({ accountId: String(accounts.list[0].id) });
  });

  const ranges: Record<Scope['range'], string> = { '30': 'Last 30 days', '90': 'Last 90 days', '365': 'Last year', all: 'All time' };
  const fill: Record<Group['outcome'], string> = { rule: 'bg-ink', none: 'bg-line-input', model: 'bg-secondary', review: 'bg-review' };
  const groupName = (g: Group) => (g.outcome === 'rule' ? g.rule_name : g.outcome === 'review' ? 'Needs review' : g.outcome === 'model' ? 'For the model to decide' : 'Left where it is');

  // Archive goes by a different name on every server; the mailbox's folder list knows which.
  const archive = $derived(cleanup.folders.find((f) => f.special_use === '\\Archive')?.name);
  const folderName = (folder: string) => (folder === 'INBOX' ? 'Inbox' : folder === archive ? 'Archive' : folder);

  const running = $derived(cleanup.phase === 'running');
  // The batch being undone; an undo moves every email of the run back, one by one, on the mail server.
  let undoing = $state<Record<number, boolean>>({});
  async function undoBatch(b: Batch) {
    undoing[b.id] = true;
    try {
      await undo(b);
    } finally {
      undoing[b.id] = false;
    }
  }
  const total = $derived(cleanup.preview?.total ?? 0);
  const max = $derived(Math.max(1, ...(cleanup.preview?.groups.map((g) => g.count) ?? [])));
  const pct = $derived(cleanup.batch?.total ? Math.round((cleanup.batch.done / cleanup.batch.total) * 100) : 0);

  // account_id is null once the mailbox is deleted.
  const mailbox = (b: Batch) => (b.account_id === null ? 'a removed mailbox' : accounts.list.find((a) => a.id === b.account_id)?.label);
  const label = (b: Batch) =>
    [mailbox(b), folderName(b.folder), b.since === null ? 'all time' : 'since ' + day(b.since)].filter(Boolean).join(' · ');
  // These count actions, not emails: a move and a mark-read on one email are two.
  const counts = (b: Batch) => {
    const parts = (['done', 'dry_run', 'failed', 'undone'] as const).filter((k) => b.actions[k]).map((k) => b.actions[k].toLocaleString() + ' ' + k.replace('_', '-'));
    return parts.length ? 'actions: ' + parts.join(' · ') : 'no actions';
  };
  const statuses: Record<Batch['status'], string> = { running: 'Running', done: 'Done', failed: 'Cut short', undone: 'Undone' };
</script>

{#snippet progress(b: Batch)}
  <div class="flex flex-col gap-1.5">
    <div role="progressbar" aria-label="Cleanup progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} class="h-3 overflow-hidden rounded bg-neutral">
      <div class="h-3 bg-ink" style:width="{pct}%"></div>
    </div>
    <div class="text-[13px] text-nav">
      {b.done.toLocaleString()} of {(b.total ?? 0).toLocaleString()} sorted · {pct}% · {b.tokens.toLocaleString()} tokens · {money(b.cost_usd)}
    </div>
  </div>
{/snippet}

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Cleanup</h1>
    <p class="mt-1 text-secondary">Apply your rules to mail that's already there. Preview first; each run can be undone as one batch.</p>
  </header>

  <section class="card flex flex-col gap-4 p-5">
    <div class="flex flex-wrap gap-3">
      <label class="flex flex-[1_1_200px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Mailbox</span>
        <select class="field h-11 px-2.5" disabled={running} value={cleanup.scope.accountId} onchange={(e) => setScope({ accountId: e.currentTarget.value })}>
          {#each accounts.list as a (a.id)}
            <option value={String(a.id)}>{a.label}</option>
          {/each}
        </select>
      </label>
      <label class="flex flex-[1_1_160px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Folder</span>
        <select class="field h-11 px-2.5" disabled={running} value={cleanup.scope.folder} onchange={(e) => setScope({ folder: e.currentTarget.value })}>
          <option value="INBOX">Inbox</option>
          {#if archive}
            <option value={archive}>Archive</option>
          {/if}
        </select>
      </label>
      <label class="flex flex-[1_1_160px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Emails from</span>
        <select class="field h-11 px-2.5" disabled={running} value={cleanup.scope.range} onchange={(e) => setScope({ range: e.currentTarget.value as Scope['range'] })}>
          {#each Object.entries(ranges) as [value, name] (value)}
            <option {value}>{name}</option>
          {/each}
        </select>
      </label>
    </div>
    <button type="button" class="btn min-h-11 self-start px-[18px] font-semibold" disabled={running || cleanup.previewing || !cleanup.scope.accountId} onclick={preview}>
      {cleanup.previewing ? 'Counting…' : cleanup.preview ? 'Refresh preview' : 'Preview what would move'}
    </button>
    {#if cleanup.previewing}
      <Waiting text="Reading your mailbox and checking each email against your rules" />
    {/if}

    {#if cleanup.preview}
      <div class="flex flex-col gap-2.5">
        <h2>
          {total.toLocaleString()} emails from {cleanup.scope.range === 'all' ? 'all time' : 'the ' + ranges[cleanup.scope.range].toLowerCase()} would be sorted like this
        </h2>
        {#each cleanup.preview.groups as g (g.key)}
          <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <span class="w-[170px] font-medium">{groupName(g)}</span>
            <div class="h-2.5 flex-[1_1_200px] overflow-hidden rounded bg-neutral">
              <div class="h-2.5 rounded {fill[g.outcome]}" style:width="{Math.max(2, Math.round((g.count / max) * 100))}%"></div>
            </div>
            <span class="w-[90px] text-right font-mono text-[13px]">{g.count.toLocaleString()}</span>
          </div>
          {#if g.samples.length}
            <ul class="flex flex-col gap-0.5 text-[12.5px] text-muted">
              {#each g.samples as m, i (i)}
                <li class="truncate"><span class="font-mono">{m.from}</span> · {m.subject}</li>
              {/each}
            </ul>
          {/if}
        {/each}
        {#if cleanup.preview.estimated_model_calls}
          <p class="text-[13px] text-secondary">
            About {cleanup.preview.estimated_model_calls.toLocaleString()} model calls{cleanup.preview.estimated_cost_usd
              ? ', around ' + money(cleanup.preview.estimated_cost_usd)
              : ''}
          </p>
        {/if}
        <p class="text-[13px] text-secondary">
          Nothing moves until you run it. Runs in the background, uses conditions and sender rules first, and can be undone as one batch.
        </p>
        {#if running && cleanup.batch}
          {@render progress(cleanup.batch)}
        {:else if running}
          <Waiting text="Listing the emails to sort" />
        {/if}
        {#if settings.value.dry_run}
          <p class="text-[13px] text-secondary">Dry-run is on: this run records what it would do and moves nothing.</p>
        {/if}
        <button type="button" class="btn-primary min-h-11 self-start px-[18px]" disabled={cleanup.phase !== 'previewed' || cleanup.previewing} onclick={run}>
          {running ? 'Sorting…' : 'Sort ' + total.toLocaleString() + ' emails'}
        </button>
      </div>
    {:else if running && cleanup.batch}
      {@render progress(cleanup.batch)}
    {:else if cleanup.phase === 'done' && cleanup.batch}
      <p role="status" class="text-[13px] text-secondary">{outcome(cleanup.batch)}</p>
    {/if}
  </section>

  <section aria-label="Batches" class="card overflow-hidden">
    <div class="px-[18px] py-3.5">
      <h2 class="text-[15px]">Batches you can undo</h2>
      <div class="text-[12.5px] text-muted">Kept for {UNDO_DAYS} days. Undo puts every email back where it was.</div>
    </div>
    {#if cleanup.status === 'error'}
      <div role="alert" class="flex flex-wrap items-center justify-between gap-2 bg-trash-bg px-[18px] py-2 text-trash">
        <span>{cleanup.error}</span>
        <button type="button" class="btn" onclick={load}>Retry</button>
      </div>
    {:else if cleanup.status === 'loading'}
      <div class="border-t border-line-divider px-[18px] py-3"><Waiting text="Loading past runs" /></div>
    {:else if cleanup.status === 'ready'}
      {#each cleanup.batches as b (b.id)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3">
          <div class="flex-[1_1_260px]">
            <div class="font-semibold">{label(b)}</div>
            <div class="text-[12.5px] text-muted">{day(b.created_at)}, {clock(b.created_at)} · {counts(b)}</div>
          </div>
          <span class="text-[12.5px] font-semibold {b.status === 'undone' ? 'text-muted' : 'text-ink'}">{statuses[b.status]}</span>
          {#if b.status === 'done' || b.status === 'failed'}
            {@const why = noUndo(b)}
            {#if why}
              <span class="text-[12.5px] text-muted">{why}</span>
            {:else}
              <button type="button" class="btn min-h-9 px-3" aria-label="Undo batch {label(b)}" disabled={undoing[b.id]} onclick={() => undoBatch(b)}>{undoing[b.id] ? 'Undoing…' : 'Undo batch'}</button>
              {#if undoing[b.id]}
                <div class="w-full"><Waiting text="Putting the emails back where they were" /></div>
              {/if}
            {/if}
          {/if}
        </div>
      {:else}
        <p class="border-t border-line-divider px-[18px] py-3 text-[13px] text-muted">No cleanup runs yet.</p>
      {/each}
      {#if cleanup.next}
        <div class="border-t border-line-divider px-[18px] py-3">
          <button type="button" class="btn px-3" onclick={more}>Show more</button>
        </div>
      {/if}
    {/if}
  </section>
</div>
