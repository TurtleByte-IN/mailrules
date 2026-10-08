<script lang="ts">
  import type { Batch, CleanupCheckRow } from '../lib/api/cleanup';
  import { TRASH_FOLDER } from '../lib/api/settings';
  import ConfirmBox from '../lib/components/ConfirmBox.svelte';
  import MailboxPicker from '../lib/components/MailboxPicker.svelte';
  import RulePicker from '../lib/components/RulePicker.svelte';
  import ScopePicker from '../lib/components/ScopePicker.svelte';
  import Waiting from '../lib/components/Waiting.svelte';
  import { clock, day, money } from '../lib/format';
  import { archiveFolder, scopeProblem } from '../lib/scope';
  import { UNDO_IGNORES_DRY_RUN } from '../lib/undo';
  import { accounts } from '../lib/state/accounts.svelte';
  import {
    acted,
    chartRows,
    chartTitle,
    check,
    cleanup,
    coverage,
    discard,
    focus,
    focused,
    load,
    more,
    noUndo,
    ruleProblem,
    runOutcome,
    runs,
    selectAll,
    selectedCount,
    selectNone,
    setMailboxes,
    setRules,
    setScope,
    sort,
    toggleRow,
    undo,
    undoable,
    undoRun,
    UNDO_DAYS,
    undoQuestion,
    undoRunQuestion,
    type Lane,
  } from '../lib/state/cleanup.svelte';
  import { load as loadRules, rules } from '../lib/state/rules.svelte';
  import { settings } from '../lib/state/settings.svelte';
  import { actionsText } from './rules/text';

  load();
  if (!rules.loaded) loadRules();

  // The mailbox list arrives after the shell mounts; start on the first one, once: picking none is the user's to do.
  let seeded = false;
  $effect(() => {
    if (seeded || !accounts.list.length) return;
    seeded = true;
    if (!cleanup.mailboxes.length) setMailboxes([String(accounts.list[0].id)]);
  });

  // The mailbox on show, whose chart and list the screen draws; with several mailboxes each has a line above it.
  const lane = $derived(focused());
  const chk = $derived(lane?.check ?? null);
  const several = $derived(cleanup.lanes.length > 1);
  const ruleError = $derived(ruleProblem());
  const problem = $derived(scopeProblem(cleanup.scope) || ruleError);
  const enabledRules = $derived(rules.list.filter((r) => r.enabled));
  // A check covers at most the daemon's newest `limits.check_max` emails of its range; say so when the range held more.
  const isCapped = (c: Lane['check']) => !!c && c.limit >= settings.value.limits.check_max && c.matched > c.total;
  const capped = $derived(isCapped(chk));
  const capText = (verb: string) => (chk ? `${verb} the newest ${chk.total.toLocaleString()} of ${chk.matched.toLocaleString()}. Run another check for the rest.` : '');

  // Archive goes by a different name on every server; the mailbox's folder list knows which.
  const archive = $derived(archiveFolder(cleanup.folders));
  const folderName = (folder: string) => (folder === 'INBOX' ? 'Inbox' : folder === archive ? 'Archive' : folder);
  const mailboxName = (id: string | number | null) => (id === null ? 'a removed mailbox' : (accounts.list.find((a) => String(a.id) === String(id))?.label ?? 'a removed mailbox'));

  const checking = $derived(cleanup.phase === 'checking');
  const sorting = $derived(cleanup.phase === 'sorting');
  const showTable = $derived(cleanup.phase === 'ready' || cleanup.phase === 'stale');
  const busy = $derived(checking || sorting);
  // Checks on show have their own mailboxes, rules, folder and range; Discard them to choose others.
  const locked = $derived(busy || showTable);

  // The batch being undone; an undo moves every email of the run back, one by one, on the mail server,
  // dry-run or not, so it is asked first (`asking`, the batch's id; `askingRun`, a run's).
  let asking = $state(0);
  let askingRun = $state(0);
  let undoing = $state<Record<number, boolean>>({});
  async function undoBatch(b: Batch) {
    asking = 0;
    undoing[b.id] = true;
    try {
      await undo(b);
    } finally {
      undoing[b.id] = false;
    }
  }
  async function undoWholeRun(bs: Batch[]) {
    askingRun = 0;
    const todo = bs.filter(undoable);
    for (const b of todo) undoing[b.id] = true;
    try {
      await undoRun(bs);
    } finally {
      for (const b of todo) undoing[b.id] = false;
    }
  }

  // The table reads over every row; the filter and paging are view-only and never change what Sort acts on,
  // except that Select all and Select none act on the rows the filter shows.
  const PAGE = 50;
  let ruleFilter = $state('');
  let pageIndex = $state(0);
  const rows = $derived(chk?.rows ?? []);
  const ruleNames = $derived([...new Set(rows.filter((r) => r.rule_name).map((r) => r.rule_name))]);
  const filtered = $derived(ruleFilter ? rows.filter((r) => r.rule_name === ruleFilter) : rows);
  const pageCount = $derived(Math.max(1, Math.ceil(filtered.length / PAGE)));
  const visible = $derived(filtered.slice(pageIndex * PAGE, pageIndex * PAGE + PAGE));
  // A filter change or a fresh check can leave the page out of range.
  $effect(() => {
    if (pageIndex >= pageCount) pageIndex = 0;
  });
  const ticked = (r: CleanupCheckRow) => r.selectable && !lane?.excluded.has(r.index);
  // Only where a rule took the email, or it waits in Needs review, is the model's confidence about a rule worth showing; a left-alone row's is not.
  const confidencePct = (r: CleanupCheckRow) => (r.confidence === null || !(r.selectable || r.review) ? '' : Math.round(r.confidence * 100) + '%');
  // Sort is about to move these: a trash goes where the setting says, so the cell names that folder.
  const trashTo = $derived(settings.value.trash_to_folder ? TRASH_FOLDER : undefined);
  const actionText = (r: CleanupCheckRow) => (r.review ? 'Needs review' : r.selectable ? actionsText(r.actions, trashTo) : '—');

  // The chart follows the ticks, so its rule rows always add up to the Sort count.
  const here = $derived(folderName(chk?.folder ?? 'INBOX'));
  const bars = $derived(chartRows(rows, lane?.excluded ?? new Set(), trashTo, here));
  const barMax = $derived(Math.max(1, ...bars.map((b) => b.count)));
  // Sorted rules take ink shades in turn; a trash, what stays and what waits each have their own colour.
  const INK = ['bg-ink', 'bg-nav', 'bg-secondary', 'bg-muted'];
  const FILL = { trash: 'bg-trash', left: 'bg-idle', review: 'bg-review' };
  // The list of emails opens under the chart on demand; a new check, or another mailbox on show, starts with it closed.
  let choosing = $state(false);
  const checkId = $derived(chk?.id);
  $effect(() => {
    void checkId;
    choosing = false;
  });
  // The rule filter belongs to the mailbox on show.
  $effect(() => {
    void lane?.accountId;
    ruleFilter = '';
    pageIndex = 0;
  });

  const pct = (b: Batch | null) => (b?.total ? Math.round((b.done / b.total) * 100) : 0);

  // account_id is null once the mailbox is deleted.
  const label = (b: Batch) => [mailboxName(b.account_id), folderName(b.folder), coverage(b)].filter(Boolean).join(' · ');
  // These count actions, not emails: a move and a mark-read on one email are two.
  const counts = (b: Batch) => {
    const parts = (['done', 'dry_run', 'failed', 'undone'] as const).filter((k) => b.actions[k]).map((k) => b.actions[k].toLocaleString() + ' ' + k.replace('_', '-'));
    return parts.length ? 'actions: ' + parts.join(' · ') : 'no actions';
  };
  const statuses: Record<Batch['status'], string> = { running: 'Running', done: 'Done', failed: 'Cut short', undone: 'Undone' };

  // What a mailbox of a run says while its check is going, and once it has gone.
  const progressLine = (c: NonNullable<Lane['check']>) => `${c.done.toLocaleString()} of ${c.total.toLocaleString()} checked · ${c.model_calls.toLocaleString()} model calls · ${money(c.cost_usd)}`;
  const laneState = (l: Lane) => {
    const c = l.check;
    if (!c) return 'Starting the check';
    if (c.status === 'running') return progressLine(c);
    if (c.status === 'failed') return 'The check failed. ' + c.error;
    if (c.status === 'stale') return 'Out of date: the rules changed since this check';
    const n = selectedCount(l);
    return n === 1 ? '1 email would be sorted' : n.toLocaleString() + ' emails would be sorted';
  };
  const batchLine = (b: Batch) =>
    `${acted(b).toLocaleString()} of ${(b.total ?? 0).toLocaleString()} ${b.actions.dry_run ? 'checked, nothing moved' : 'sorted'}${b.skipped ? ' · ' + b.skipped.toLocaleString() + ' skipped' : ''}`;
</script>

{#snippet progress(b: Batch)}
  <div class="flex flex-col gap-1.5">
    <div role="progressbar" aria-label="Cleanup progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct(b)} class="h-3 overflow-hidden rounded bg-neutral">
      <div class="h-3 bg-ink" style:width="{pct(b)}%"></div>
    </div>
    <div class="text-[13px] text-nav">
      {acted(b).toLocaleString()} of {(b.total ?? 0).toLocaleString()} sorted · {pct(b)}%{b.skipped ? ' · ' + b.skipped.toLocaleString() + ' skipped' : ''} · {b.tokens.toLocaleString()} tokens · {money(b.cost_usd)}
    </div>
  </div>
{/snippet}

{#snippet undoControls(b: Batch)}
  {#if b.status === 'done' || b.status === 'failed'}
    {@const why = noUndo(b)}
    {#if why}
      <span class="text-[12.5px] text-muted">{why}</span>
    {:else}
      <button type="button" class="btn min-h-9 px-3" aria-label="Undo batch {label(b)}" aria-expanded={asking === b.id} disabled={undoing[b.id]} onclick={() => (asking = b.id)}>{undoing[b.id] ? 'Undoing…' : 'Undo batch'}</button>
      {#if asking === b.id}
        <ConfirmBox question={undoQuestion(b)} note={UNDO_IGNORES_DRY_RUN} confirm="Yes, undo this batch" onconfirm={() => undoBatch(b)} oncancel={() => (asking = 0)} />
      {/if}
      {#if undoing[b.id]}
        <div class="w-full"><Waiting text="Putting the emails back where they were" /></div>
      {/if}
    {/if}
  {/if}
{/snippet}

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Cleanup</h1>
    <p class="mt-1 text-secondary">
      Apply your rules to mail that's already there, on one mailbox or several, with every rule or only some. Check what would move, untick anything you want to keep, then sort it as one undoable batch per mailbox.
    </p>
  </header>

  <section class="card flex flex-col gap-4 p-5">
    {#if accounts.list.length > 1}
      <MailboxPicker picked={cleanup.mailboxes} disabled={locked} onchange={setMailboxes} />
    {/if}
    {#if enabledRules.length > 1}
      <RulePicker ids={cleanup.ruleIds} rules={enabledRules} disabled={locked} onchange={setRules} />
      {#if ruleError}
        <p role="alert" class="-mt-2 text-[12.5px] text-trash">{ruleError}</p>
      {/if}
    {/if}

    <ScopePicker id="cleanup" scope={cleanup.scope} folders={cleanup.folders} {busy} {locked} mailbox={false} onchange={setScope} />
    {#if cleanup.mailboxes.length > 1}
      <p class="-mt-2 text-[12.5px] text-muted">With several mailboxes, mail is taken from each Inbox.</p>
    {/if}

    <button type="button" class="btn min-h-11 self-start px-[18px] font-semibold" disabled={locked || !cleanup.mailboxes.length || !!problem} onclick={check}>
      {checking ? 'Checking…' : 'Check what would move'}
    </button>

    {#if checking && several}
      <ul aria-label="Checking each mailbox" class="flex flex-col gap-1.5">
        {#each cleanup.lanes as l (l.accountId)}
          <li class="text-[13px] text-nav"><span class="font-semibold">{mailboxName(l.accountId)}</span> · {laneState(l)}</li>
        {/each}
      </ul>
      <Waiting text="Reading your mailboxes and checking each email against your rules" />
    {:else if checking && chk}
      <div class="flex flex-col gap-2">
        <p class="text-[13px] text-nav">{progressLine(chk)}</p>
        {#if capped}
          <p class="text-[13px] text-secondary">{capText('Checking')}</p>
        {/if}
        <Waiting text="Reading your mailbox and checking each email against your rules" />
      </div>
    {:else if checking}
      <Waiting text="Reading your mailbox and checking each email against your rules" />
    {/if}

    {#if cleanup.phase === 'failed'}
      {#if several}
        <ul aria-label="Mailboxes" class="flex flex-col gap-1.5">
          {#each cleanup.lanes as l (l.accountId)}
            <li role="alert" class="text-[13px] text-trash"><span class="font-semibold">{mailboxName(l.accountId)}</span> · {laneState(l)}</li>
          {/each}
        </ul>
      {:else}
        <p role="alert" class="text-[13px] text-trash">The check failed. {chk?.error}</p>
      {/if}
    {/if}

    {#if showTable && chk}
      <div class="flex min-w-0 flex-col gap-3">
        {#if cleanup.phase === 'stale'}
          <p role="alert" class="rounded bg-trash-bg px-3 py-2 text-[13px] text-trash">
            These results are out of date: the rules changed since this check. Check again before sorting.
          </p>
        {/if}

        {#if several}
          <!-- One line per mailbox of the run; the chart and list below are of the one on show. -->
          <ul aria-label="Mailboxes in this run" class="flex flex-col divide-y divide-line-divider rounded border border-line-divider">
            {#each cleanup.lanes as l (l.accountId)}
              {@const on = l.accountId === cleanup.scope.accountId}
              <li class="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 px-3 py-2 text-[13px] {on ? 'bg-selected-row' : ''}">
                <span class="min-w-0 flex-[1_1_200px]">
                  <span class="font-semibold break-all">{mailboxName(l.accountId)}</span>
                  <span class={l.check?.status === 'failed' ? 'text-trash' : 'text-secondary'}> · {laneState(l)}</span>
                </span>
                <button type="button" class="btn min-h-9 px-3" aria-pressed={on} disabled={l.check?.status === 'failed'} onclick={() => focus(l.accountId)}>{on ? 'Shown below' : 'Show'}</button>
              </li>
            {/each}
          </ul>
        {/if}

        {#if capped}
          <p class="text-[13px] text-secondary">{capText('Checked')}</p>
        {/if}

        <div class="flex flex-col gap-2.5">
          <h2 class="text-[17px]">{several ? mailboxName(lane?.accountId ?? '') + ': ' : ''}{chartTitle(chk, here)}</h2>
          {#if rows.length}
            <ul aria-label="What the check would do" class="flex flex-col gap-2.5">
              {#each bars as b, i (b.name)}
                <!-- On a phone the name takes its own line over the bar and the count. -->
                <li class="grid grid-cols-[minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1 md:grid-cols-[minmax(0,14rem)_minmax(0,1fr)_5.5rem]">
                  <span class="break-words font-medium max-md:col-span-2">{b.name}</span>
                  <div class="h-2.5 overflow-hidden rounded-[3px] bg-neutral">
                    <div class="h-2.5 rounded-[3px] {b.kind === 'rule' ? INK[i % INK.length] : FILL[b.kind]}" style:width="{b.count ? Math.max(2, Math.round((b.count / barMax) * 100)) : 0}%"></div>
                  </div>
                  <span class="text-right font-mono text-[13px]">{b.count.toLocaleString()}</span>
                </li>
              {/each}
            </ul>
          {/if}
        </div>

        <p class="text-[13px] text-secondary">
          Nothing moves until you sort. Sort runs in the background, does what {several ? 'these checks' : 'this check'} found for each ticked email without asking the model again, and can be undone as one batch{several ? ' per mailbox' : ''}.
        </p>
        {#if settings.value.dry_run}
          <p class="text-[13px] text-secondary">Dry-run is on: this run records what it would do and moves nothing.</p>
        {/if}

        <div class="flex flex-wrap items-center gap-3">
          <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={cleanup.phase !== 'ready' || selectedCount() === 0} onclick={sort}>
            Sort {selectedCount().toLocaleString()} {selectedCount() === 1 ? 'email' : 'emails'}
          </button>
          <button type="button" class="btn min-h-11 px-[18px]" aria-expanded={choosing} aria-controls="cleanup-emails" onclick={() => (choosing = !choosing)}>
            {choosing ? 'Hide emails' : 'Choose emails'}
          </button>
          <button type="button" class="btn min-h-11 px-[18px]" onclick={discard}>{several ? 'Discard checks' : 'Discard check'}</button>
        </div>

        {#if choosing}
          <div id="cleanup-emails" class="flex min-w-0 flex-col gap-3">
            <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
              <button type="button" class="btn min-h-9 px-3" onclick={() => selectAll(ruleFilter)}>Select all</button>
              <button type="button" class="btn min-h-9 px-3" onclick={() => selectNone(ruleFilter)}>Select none</button>
              {#if ruleNames.length}
                <label class="flex items-center gap-1.5 text-[13px] max-md:w-full">
                  <span>Rule</span>
                  <select class="field h-9 px-2 max-md:min-w-0 max-md:flex-1" value={ruleFilter} onchange={(e) => (ruleFilter = e.currentTarget.value)}>
                    <option value="">All rules</option>
                    {#each ruleNames as name (name)}
                      <option value={name}>{name}</option>
                    {/each}
                  </select>
                </label>
              {/if}
              <span class="text-[13px] text-secondary">{selectedCount(lane).toLocaleString()} selected of {rows.length.toLocaleString()}</span>
            </div>

            <!-- A short box that scrolls inside the card, its header row staying in view. -->
            <div class="max-h-[420px] overflow-y-auto rounded border border-line-divider px-3 max-md:max-h-[60vh]">
              <!-- On a phone each row stacks: from and confidence, the subject, then the rule and the action. -->
              <table class="w-full table-fixed text-[13px] max-md:block">
                <colgroup>
                  <col class="w-8" />
                  <col class="w-[20%]" />
                  <col />
                  <col class="w-[16%]" />
                  <col class="w-[16%]" />
                  <col class="w-[88px]" />
                </colgroup>
                <!-- A collapsed border scrolls away under a sticky head, so the divider is drawn as a shadow. -->
                <thead class="sticky top-0 z-10 bg-surface shadow-[inset_0_-1px_0_var(--color-line-divider)] max-md:hidden">
                  <tr class="text-left text-muted">
                    <th class="py-2"><span class="sr-only">Selected</span></th>
                    <th class="py-2 pr-2 font-medium">From</th>
                    <th class="py-2 pr-2 font-medium">Subject</th>
                    <th class="py-2 pr-2 font-medium">Rule</th>
                    <th class="py-2 pr-2 font-medium">Action</th>
                    <th class="py-2 text-right font-medium">Confidence</th>
                  </tr>
                </thead>
                <tbody class="max-md:block">
                  {#each visible as r (r.index)}
                    <tr class="border-b border-line-divider align-top last:border-b-0 max-md:grid max-md:grid-cols-[2rem_minmax(0,1fr)_auto] max-md:gap-x-2 max-md:gap-y-0.5 max-md:py-2">
                      <td class="py-2 max-md:row-span-4 max-md:py-0">
                        <!-- The label makes the whole cell the tap target on a phone. -->
                        <label class="max-md:flex max-md:h-full max-md:min-h-11 max-md:items-start max-md:pt-0.5">
                          <input
                            type="checkbox"
                            aria-label="Sort {r.from} · {r.subject}"
                            checked={ticked(r)}
                            disabled={!r.selectable}
                            title={r.selectable ? '' : r.reason}
                            onchange={() => toggleRow(r.index)}
                          />
                        </label>
                      </td>
                      <td class="truncate py-2 pr-2 font-mono max-md:col-start-2 max-md:row-start-1 max-md:py-0" title={r.from}>{r.from}</td>
                      <td class="py-2 pr-2 max-md:col-span-2 max-md:col-start-2 max-md:row-start-2 max-md:py-0 max-md:pr-0">
                        <div class="truncate" title={r.subject}>{r.subject}</div>
                        {#if !r.selectable && r.reason}
                          <div class="break-words text-[12px] text-muted">{r.reason}</div>
                        {/if}
                      </td>
                      <td class="truncate py-2 pr-2 max-md:col-span-2 max-md:col-start-2 max-md:row-start-3 max-md:py-0 max-md:text-secondary" title={r.rule_name}>{r.rule_name || '—'}</td>
                      <td class="truncate py-2 pr-2 max-md:col-span-2 max-md:col-start-2 max-md:row-start-4 max-md:py-0 max-md:text-secondary" title={actionText(r)}>{actionText(r)}</td>
                      <td class="py-2 text-right font-mono max-md:col-start-3 max-md:row-start-1 max-md:py-0">{confidencePct(r)}</td>
                    </tr>
                  {/each}
                </tbody>
              </table>
            </div>

            {#if rows.length > PAGE}
              <div class="flex items-center gap-3 text-[13px]">
                <button type="button" class="btn min-h-9 px-3" disabled={pageIndex === 0} onclick={() => (pageIndex -= 1)}>Previous</button>
                <span class="text-secondary">Page {pageIndex + 1} of {pageCount}</span>
                <button type="button" class="btn min-h-9 px-3" disabled={pageIndex >= pageCount - 1} onclick={() => (pageIndex += 1)}>Next</button>
              </div>
            {/if}
          </div>
        {/if}
      </div>
    {:else if sorting && several}
      <ul aria-label="Sorting each mailbox" class="flex flex-col gap-3">
        {#each cleanup.lanes as l (l.accountId)}
          <li class="flex flex-col gap-1">
            <span class="text-[13px] font-semibold">{mailboxName(l.accountId)}</span>
            {#if l.batch}
              {@render progress(l.batch)}
            {:else}
              <Waiting text="Listing the emails to sort" />
            {/if}
          </li>
        {/each}
      </ul>
    {:else if sorting && lane?.batch}
      {@render progress(lane.batch)}
    {:else if sorting}
      <Waiting text="Listing the emails to sort" />
    {:else if cleanup.phase === 'done' && lane?.batch}
      {@const finished = cleanup.lanes.flatMap((l) => (l.batch ? [l.batch] : []))}
      <p role="status" class="text-[13px] text-secondary">{runOutcome(finished)}</p>
      {#if several}
        <ul aria-label="Mailboxes in this run" class="flex flex-col gap-1 text-[13px] text-secondary">
          {#each finished as b (b.id)}
            <li><span class="font-semibold">{mailboxName(b.account_id)}</span> · {batchLine(b)}</li>
          {/each}
        </ul>
      {/if}
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
      {#each runs(cleanup.batches) as run (run.id)}
        {#if run.batches.length === 1}
          {@const b = run.batches[0]}
          <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3">
            <div class="flex-[1_1_260px]">
              <div class="font-semibold">{label(b)}</div>
              <div class="text-[12.5px] text-muted">{day(b.created_at)}, {clock(b.created_at)} · {counts(b)}</div>
            </div>
            <span class="text-[12.5px] font-semibold {b.status === 'undone' ? 'text-muted' : 'text-ink'}">{statuses[b.status]}</span>
            {@render undoControls(b)}
          </div>
        {:else}
          {@const first = run.batches[0]}
          <div class="flex flex-col border-t border-line-divider" role="group" aria-label="Run over {run.batches.length} mailboxes, {day(first.created_at)}, {clock(first.created_at)}">
            <div class="flex flex-wrap items-center gap-x-4 gap-y-2 px-[18px] py-3">
              <div class="flex-[1_1_260px]">
                <div class="font-semibold">Run over {run.batches.length} mailboxes · {folderName(first.folder)} · {coverage(first)}</div>
                <div class="text-[12.5px] text-muted">{day(first.created_at)}, {clock(first.created_at)}</div>
              </div>
              {#if run.batches.some(undoable)}
                <button type="button" class="btn min-h-9 px-3" aria-label="Undo the whole run" aria-expanded={askingRun === run.id} disabled={run.batches.some((b) => undoing[b.id])} onclick={() => (askingRun = run.id)}>
                  Undo whole run
                </button>
                {#if askingRun === run.id}
                  <ConfirmBox question={undoRunQuestion(run.batches)} note={UNDO_IGNORES_DRY_RUN} confirm="Yes, undo this run" onconfirm={() => undoWholeRun(run.batches)} oncancel={() => (askingRun = 0)} />
                {/if}
              {/if}
            </div>
            {#each run.batches as b (b.id)}
              <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider py-2.5 pr-[18px] pl-8">
                <div class="flex-[1_1_240px]">
                  <div class="font-semibold">{mailboxName(b.account_id)}</div>
                  <div class="text-[12.5px] text-muted">{counts(b)}</div>
                </div>
                <span class="text-[12.5px] font-semibold {b.status === 'undone' ? 'text-muted' : 'text-ink'}">{statuses[b.status]}</span>
                {@render undoControls(b)}
              </div>
            {/each}
          </div>
        {/if}
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
