<script lang="ts">
  import type { Batch, Preview } from '../lib/api/cleanup';
  import NotBuilt from '../lib/components/NotBuilt.svelte';
  import { clock, day } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { cleanup, outcome, preview, run, setScope, undo, type Scope } from '../lib/state/cleanup.svelte';

  type Group = Preview['groups'][number];

  // The mailbox list arrives after the shell mounts; start on the first one.
  $effect(() => {
    if (!cleanup.scope.accountId && accounts.list.length) setScope({ accountId: String(accounts.list[0].id) });
  });

  const ranges: Record<Scope['range'], string> = { '30': 'Last 30 days', '90': 'Last 90 days', '365': 'Last year', all: 'All time' };
  const fill: Record<Group['outcome'], string> = { rule: 'bg-ink', none: 'bg-line-input', review: 'bg-review' };
  const groupName = (g: Group) => (g.outcome === 'rule' ? g.rule_name : g.outcome === 'review' ? 'Needs review' : 'Left where it is');

  // Archive goes by a different name on every server; the mailbox's folder list knows which.
  const archive = $derived(cleanup.folders.find((f) => f.special_use === '\\Archive')?.name);
  const folderName = (folder: string) => (folder === 'INBOX' ? 'Inbox' : folder === archive ? 'Archive' : folder);

  const running = $derived(cleanup.phase === 'running');
  const total = $derived(cleanup.preview?.total ?? 0);
  const max = $derived(Math.max(1, ...(cleanup.preview?.groups.map((g) => g.count) ?? [])));
  const pct = $derived(cleanup.batch?.total ? Math.round((cleanup.batch.done / cleanup.batch.total) * 100) : 0);

  const scopeLabel = (s: Scope | null) =>
    s ? [accounts.list.find((a) => String(a.id) === s.accountId)?.label, folderName(s.folder), ranges[s.range].toLowerCase()].join(' · ') : '';
  const actions = (b: Batch) => b.actions.done + b.actions.dry_run + b.actions.failed + b.actions.undone;
  const statusName = (b: Batch) => (b.status === 'undone' ? 'Undone' : b.status === 'failed' ? 'Failed' : 'Done');
</script>

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Cleanup</h1>
    <p class="mt-1 text-secondary">Apply your rules to mail that's already there. Preview first; each run can be undone as one batch.</p>
  </header>

  {#if cleanup.notBuilt}
    <NotBuilt what="Cleanup" />
  {:else}
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
      <button type="button" class="btn min-h-11 self-start px-[18px] font-semibold" disabled={running || !cleanup.scope.accountId} onclick={preview}>
        {cleanup.preview ? 'Refresh preview' : 'Preview what would move'}
      </button>

      {#if cleanup.preview}
        <div class="flex flex-col gap-2.5">
          <h2>
            {total.toLocaleString()} emails from {cleanup.scope.range === 'all' ? 'all time' : 'the ' + ranges[cleanup.scope.range].toLowerCase()} would be sorted like this
          </h2>
          {#each cleanup.preview.groups as g (g.outcome + g.rule_id)}
            <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
              <span class="w-[170px] font-medium">{groupName(g)}</span>
              <div class="h-2.5 flex-[1_1_200px] overflow-hidden rounded bg-neutral">
                <div class="h-2.5 rounded {fill[g.outcome]}" style:width="{Math.max(2, Math.round((g.count / max) * 100))}%"></div>
              </div>
              <span class="w-[90px] text-right font-mono text-[13px]">{g.count.toLocaleString()}</span>
            </div>
          {/each}
          <p class="text-[13px] text-secondary">
            Nothing moves until you run it. Runs in the background, uses conditions and sender rules first, and can be undone as one batch.
          </p>
          {#if running && cleanup.batch}
            <div class="flex flex-col gap-1.5">
              <div
                role="progressbar"
                aria-label="Cleanup progress"
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={pct}
                class="h-3 overflow-hidden rounded bg-neutral"
              >
                <div class="h-3 bg-ink" style:width="{pct}%"></div>
              </div>
              <div class="text-[13px] text-nav">
                {cleanup.batch.done.toLocaleString()} of {(cleanup.batch.total ?? total).toLocaleString()} sorted · {pct}%
              </div>
            </div>
          {/if}
          <button type="button" class="btn-primary min-h-11 self-start px-[18px]" disabled={cleanup.phase !== 'previewed'} onclick={() => run()}>
            {running ? 'Sorting…' : 'Sort ' + total.toLocaleString() + ' emails'}
          </button>
        </div>
      {:else if cleanup.phase === 'done' && cleanup.batch}
        <p role="status" class="text-[13px] text-secondary">{outcome(cleanup.batch)}</p>
      {/if}
    </section>

    {#if cleanup.batch && cleanup.batch.status !== 'running'}
      {@const b = cleanup.batch}
      <section aria-label="Batches" class="card overflow-hidden">
        <div class="px-[18px] py-3.5">
          <h2 class="text-[15px]">Batches you can undo</h2>
          <div class="text-[12.5px] text-muted">Undo puts every email back where it was.</div>
        </div>
        <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3">
          <div class="flex-[1_1_260px]">
            <div class="font-semibold">{scopeLabel(cleanup.batchScope)}</div>
            <div class="text-[12.5px] text-muted">{day(b.created_at)}, {clock(b.created_at)} · {actions(b).toLocaleString()} actions</div>
          </div>
          <span class="text-[12.5px] font-semibold {b.status === 'undone' ? 'text-muted' : 'text-ink'}">{statusName(b)}</span>
          {#if b.status !== 'undone'}
            <button type="button" class="btn min-h-9 px-3" aria-label="Undo batch {scopeLabel(cleanup.batchScope)}" onclick={undo}>Undo batch</button>
          {/if}
        </div>
      </section>
    {/if}
  {/if}
</div>
