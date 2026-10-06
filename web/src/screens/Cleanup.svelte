<script lang="ts">
  import type { PreviewRow, Scope } from '../lib/api/cleanup';
  import { clock, day, money } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { cleanup, load, preview, run, setScope, undo } from '../lib/state/cleanup.svelte';

  load();

  // The mailbox list arrives after the shell mounts; start on the first one.
  $effect(() => {
    if (!cleanup.scope.accountId && accounts.list.length) setScope({ accountId: accounts.list[0].id });
  });

  const folders: Record<Scope['folder'], string> = { INBOX: 'Inbox', Archive: 'Archive', all: 'All folders' };
  const ranges: Record<Scope['range'], string> = { '30': 'Last 30 days', '90': 'Last 90 days', '365': 'Last year', all: 'All time' };
  const fill: Record<PreviewRow['kind'], string> = { rule: 'bg-ink', trash: 'bg-trash', inbox: 'bg-line-input', review: 'bg-review' };

  const running = $derived(cleanup.phase === 'running');
  const total = $derived(cleanup.preview?.total ?? 0);
  const max = $derived(Math.max(1, ...(cleanup.preview?.rows.map((r) => r.count) ?? [])));
  const pct = $derived(cleanup.batch?.total ? Math.round((cleanup.batch.done / cleanup.batch.total) * 100) : 0);

  const scopeLabel = (s: Scope) =>
    [accounts.list.find((a) => a.id === s.accountId)?.email, folders[s.folder], ranges[s.range].toLowerCase()].join(' · ');
</script>

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
            <option value={a.id}>{a.email}</option>
          {/each}
        </select>
      </label>
      <label class="flex flex-[1_1_160px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Folder</span>
        <select class="field h-11 px-2.5" disabled={running} value={cleanup.scope.folder} onchange={(e) => setScope({ folder: e.currentTarget.value as Scope['folder'] })}>
          {#each Object.entries(folders) as [value, name] (value)}
            <option {value}>{name}</option>
          {/each}
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
    <button type="button" class="btn min-h-11 self-start px-[18px] font-semibold" disabled={running} onclick={preview}>
      {cleanup.preview ? 'Refresh preview' : 'Preview what would move'}
    </button>

    {#if cleanup.preview}
      <div class="flex flex-col gap-2.5">
        <h2>
          {total.toLocaleString()} emails from {cleanup.scope.range === 'all' ? 'all time' : 'the ' + ranges[cleanup.scope.range].toLowerCase()} would be sorted like this
        </h2>
        {#each cleanup.preview.rows as r (r.name)}
          <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
            <span class="w-[170px] font-medium">{r.name}</span>
            <div class="h-2.5 flex-[1_1_200px] overflow-hidden rounded bg-neutral">
              <div class="h-2.5 rounded {fill[r.kind]}" style:width="{Math.max(2, Math.round((r.count / max) * 100))}%"></div>
            </div>
            <span class="w-[90px] text-right font-mono text-[13px]">{r.count.toLocaleString()}</span>
            {#each r.samples as s (s.subject)}
              <div class="w-full truncate text-[12.5px] text-muted">{s.sender} · {s.subject}</div>
            {/each}
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
              {cleanup.batch.done.toLocaleString()} of {cleanup.batch.total.toLocaleString()} sorted · {pct}% · {cleanup.batch.tokens.toLocaleString()} tokens · {money(cleanup.batch.costUsd)}
            </div>
          </div>
        {/if}
        <button type="button" class="btn-primary min-h-11 self-start px-[18px]" disabled={cleanup.phase !== 'previewed'} onclick={() => run()}>
          {running ? 'Sorting…' : 'Sort ' + total.toLocaleString() + ' emails'}
        </button>
      </div>
    {:else if cleanup.phase === 'done' && cleanup.batch}
      <p role="status" class="text-[13px] text-secondary">
        Cleanup done: {cleanup.batch.total.toLocaleString()} emails sorted. Undo it as one batch below.
      </p>
    {/if}
  </section>

  <section aria-label="Batches" class="card overflow-hidden">
    <div class="px-[18px] py-3.5">
      <h2 class="text-[15px]">Batches you can undo</h2>
      <div class="text-[12.5px] text-muted">Kept for 30 days. Undo puts every email back where it was.</div>
    </div>
    {#each cleanup.batches as b (b.id)}
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3">
        <div class="flex-[1_1_260px]">
          <div class="font-semibold">{scopeLabel(b.scope)}</div>
          <div class="text-[12.5px] text-muted">{day(b.createdAt)}, {clock(b.createdAt)} · {b.total.toLocaleString()} actions</div>
        </div>
        <span class="text-[12.5px] font-semibold {b.status === 'undone' ? 'text-muted' : 'text-ink'}">{b.status === 'undone' ? 'Undone' : 'Done'}</span>
        {#if b.status !== 'undone'}
          <button type="button" class="btn min-h-9 px-3" aria-label="Undo batch {scopeLabel(b.scope)}" onclick={() => undo(b.id)}>Undo batch</button>
        {/if}
      </div>
    {/each}
  </section>
</div>
