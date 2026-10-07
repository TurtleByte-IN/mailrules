<script lang="ts">
  import ScopePicker from '../../lib/components/ScopePicker.svelte';
  import Waiting from '../../lib/components/Waiting.svelte';
  import { money } from '../../lib/format';
  import { scopeProblem } from '../../lib/scope';
  import { accounts } from '../../lib/state/accounts.svelte';
  import { again, createSelected, discard, samplesProblem, scan, selected, setSamples, setScope, suggest, type Card } from '../../lib/state/suggest.svelte';
  import RuleLines from './RuleLines.svelte';

  // The mailbox list arrives after the shell mounts; start on the first one.
  $effect(() => {
    if (!suggest.scope.accountId && accounts.list.length) setScope({ accountId: String(accounts.list[0].id) });
  });

  const plural = (n: number, one: string, many: string) => `${n.toLocaleString()} ${n === 1 ? one : many}`;

  const scanning = $derived(suggest.phase === 'scanning');
  const problem = $derived(scopeProblem(suggest.scope));
  const samples = $derived(samplesProblem());
  const typed = (e: Event & { currentTarget: HTMLInputElement }) => (Number.isNaN(e.currentTarget.valueAsNumber) ? null : e.currentTarget.valueAsNumber);

  const bodies = { none: 'None', first500: 'First 500 characters', full: 'Full body' } as const;
  const bodyTails = { none: '. No email text.', first500: ' plus the first 500 characters of each of those emails.', full: ' plus the full text of each of those emails.' } as const;
  const howMany = $derived(
    suggest.samplesMode === 'all' ? 'all emails' : samples ? 'the chosen number of emails' : `up to ${plural(suggest.upTo!, 'email', 'emails')}`,
  );
  const sentNote = $derived(`Sent to your AI model provider: each sender's address, name and email counts, and the subject and date of ${howMany} per sender${bodyTails[suggest.body]}`);

  const p = $derived(suggest.progress);
  const pct = $derived(p?.total ? Math.round((p.read / p.total) * 100) : 0);
  const step = $derived(
    !p ? '' : p.phase === 'reading' ? `${p.read.toLocaleString()} of ${p.total.toLocaleString()} emails read` : p.phase === 'asking' ? `Asking the AI: request ${Math.min(p.requests_done + 1, p.requests_total)} of ${p.requests_total}` : 'Merging the suggestions',
  );

  const result = $derived(suggest.result);
  const ticked = $derived(selected().length);

  let creating = $state(false);
  async function create() {
    creating = true;
    try {
      await createSelected();
    } finally {
      creating = false;
    }
  }

  // A refused create stays on its card until the card is changed.
  const change = (c: Card, patch: Partial<Card>) => Object.assign(c, patch, { refused: '', refusedName: false });

  const senders = (c: Card) =>
    c.groups
      .slice(0, 5)
      .map((g) => `${g.from} (${g.count.toLocaleString()})`)
      .join(', ') + (c.groups.length > 5 ? ` +${c.groups.length - 5} more` : '');
</script>

<div class="flex flex-col gap-[18px]">
  <section class="card flex flex-col gap-4 p-5">
    <p class="text-[13.5px] text-secondary">MailRules reads the emails you pick, groups them by sender, and asks the AI to suggest rules. Nothing is moved: you choose which rules to create.</p>

    <ScopePicker id="suggest" scope={suggest.scope} folders={suggest.folders} busy={scanning} onchange={setScope} />

    <div class="flex flex-wrap gap-3">
      <label class="flex flex-[1_1_160px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Samples per sender</span>
        <select class="field h-11 px-2.5" disabled={scanning} value={suggest.samplesMode} onchange={(e) => setSamples({ samplesMode: e.currentTarget.value as 'upto' | 'all' })}>
          <option value="upto">Up to</option>
          <option value="all">All of them</option>
        </select>
      </label>
      {#if suggest.samplesMode === 'upto'}
        <div class="flex flex-[0_1_140px] flex-col gap-1.5">
          <span class="text-[13px] font-semibold">Samples</span>
          <input
            type="number"
            min="1"
            step="1"
            inputmode="numeric"
            aria-label="How many samples per sender"
            aria-invalid={samples ? true : undefined}
            aria-describedby={samples ? 'suggest-samples-problem' : undefined}
            class="field h-11 px-2.5 font-mono"
            disabled={scanning}
            value={suggest.upTo}
            oninput={(e) => setSamples({ upTo: typed(e) })}
          />
        </div>
      {/if}
      <label class="flex flex-[1_1_200px] flex-col gap-1.5">
        <span class="text-[13px] font-semibold">Body sent to the AI</span>
        <select class="field h-11 px-2.5" disabled={scanning} value={suggest.body} onchange={(e) => (suggest.body = e.currentTarget.value as keyof typeof bodies)}>
          {#each Object.entries(bodies) as [value, name] (value)}
            <option {value}>{name}</option>
          {/each}
        </select>
      </label>
    </div>

    {#if samples}
      <p id="suggest-samples-problem" role="alert" class="-mt-2 text-[12.5px] text-trash">{samples}</p>
    {/if}
    <p role="none" class="-mt-1 text-[12.5px] text-secondary">{sentNote}</p>

    <button type="button" class="btn-primary min-h-11 self-start px-[18px]" disabled={scanning || !suggest.scope.accountId || !!problem || !!samples} onclick={scan}>
      {scanning ? 'Suggesting…' : 'Suggest rules'}
    </button>

    {#if scanning && p}
      <div class="flex flex-col gap-1.5">
        {#if p.phase === 'reading'}
          <div role="progressbar" aria-label="Scan progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} class="h-3 overflow-hidden rounded bg-neutral">
            <div class="h-3 bg-ink" style:width="{pct}%"></div>
          </div>
        {/if}
        <div role="status" class="text-[13px] text-nav">{step} · {plural(p.tokens, 'token', 'tokens')} · {money(p.cost_usd)}</div>
        {#if p.matched > p.total}
          <p class="text-[13px] text-secondary">Scanning the newest {p.total.toLocaleString()} of {p.matched.toLocaleString()}.</p>
        {/if}
      </div>
    {:else if scanning}
      <Waiting text="Reading your mailbox" />
    {/if}

    {#if suggest.needsModel}
      <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{suggest.needsModel} <a href="#/settings" class="font-semibold underline">Open Settings</a></div>
    {/if}
  </section>

  {#if suggest.phase === 'created'}
    <section class="card flex flex-col gap-3 p-5">
      <p role="status" class="text-[13.5px]">
        Created {plural(suggest.created, 'rule', 'rules')}. {suggest.created === 1 ? 'It sorts' : 'They sort'} new mail from now on; the emails already in your mailbox stay where they are.
      </p>
      <div class="flex flex-wrap items-center gap-3">
        <a href="#/cleanup" class="font-semibold underline">Sort existing mail in Cleanup</a>
        <button type="button" class="btn" onclick={again}>Suggest again</button>
      </div>
    </section>
  {:else if result && suggest.phase !== 'scanning'}
    <div class="flex flex-col gap-3.5">
      <div class="flex flex-wrap items-center justify-between gap-2.5">
        <div class="flex flex-col gap-1">
          <h2>{plural(suggest.cards.length, 'suggestion', 'suggestions')} from {plural(result.scanned, 'email', 'emails')}</h2>
          <p class="text-[12.5px] text-secondary">
            {plural(result.groups, 'sender', 'senders')} · {plural(result.requests, 'AI request', 'AI requests')} · {plural(result.tokens, 'token', 'tokens')} · {money(result.cost_usd)}
          </p>
        </div>
        <div class="flex gap-2">
          <button type="button" class="btn" disabled={creating} onclick={discard}>Discard</button>
          <button type="button" class="btn-primary" disabled={creating || !ticked} onclick={create}>{creating ? 'Creating…' : `Create ${ticked} selected`}</button>
        </div>
      </div>
      {#if result.notes.length}
        <ul role="status" class="flex list-disc flex-col gap-0.5 pl-5 text-[13px] text-secondary">
          {#each result.notes as n}
            <li>{n}</li>
          {/each}
        </ul>
      {/if}
      {#if !suggest.cards.length}
        <p class="text-[13.5px] text-secondary">The AI found nothing worth a rule in these emails.</p>
      {/if}
      {#each suggest.cards as c}
        {@const exact = c.kind === 'exact'}
        {@const scanned = plural(result.scanned, 'scanned email', 'scanned emails')}
        <article
          class="flex flex-col gap-3 rounded-md border bg-surface p-[18px] {c.trashes ? 'border-trash border-l-[6px]' : 'border-line-input'} {c.errors.length ? 'border-dashed opacity-60' : ''}"
        >
          <div class="flex flex-wrap items-center gap-2">
            <input
              type="checkbox"
              class="h-[18px] w-[18px]"
              aria-label={`Create ${c.name}`}
              checked={c.ticked && !c.errors.length}
              disabled={!!c.errors.length}
              onchange={(e) => change(c, { ticked: e.currentTarget.checked })}
            />
            <!-- Editable: a name the daemon refuses (taken, or twice in one create) can only be fixed here. -->
            <input class="field h-9 w-[220px] max-w-full text-base font-semibold" aria-label="Rule name" aria-invalid={c.refusedName || undefined} value={c.name} oninput={(e) => change(c, { name: e.currentTarget.value })} />
            <span class={exact ? 'chip chip-neutral' : 'chip'}>{exact ? 'Exact · no AI needed' : 'By meaning · AI decides each email'}</span>
            {#if c.trashes}<span class="chip chip-trash">Trash</span>{/if}
          </div>
          {#if c.reason}
            <p class="text-[13.5px] text-secondary">{c.reason}</p>
          {/if}
          {#if c.trashes}
            <p class="rounded bg-trash-bg px-3 py-2 text-[13px] font-semibold text-trash">Trashes {c.match_count.toLocaleString()} of the scanned {c.match_count === 1 ? 'email' : 'emails'}</p>
          {/if}
          <RuleLines rule={c} />
          {#if c.new_folders.length}
            <div class="text-[13px]">Creates {c.new_folders.length === 1 ? 'folder' : 'folders'} {c.new_folders.join(', ')}</div>
          {/if}
          {#each c.conflicts as x}
            <div class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{x.note}</div>
          {/each}
          {#each c.errors as e}
            <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{e.message}</div>
          {/each}
          <div class="text-[12.5px] text-secondary">
            {#if exact}
              Matches {c.match_count.toLocaleString()} of the {scanned}
            {:else}
              The AI grouped {c.match_count.toLocaleString()} of the {scanned} here <span class="text-muted">(its own guess: a rule by meaning is decided by the AI on each new email)</span>
            {/if}
          </div>
          {#if c.samples.length}
            <div class="text-[12.5px] {c.trashes ? 'text-trash' : 'text-muted'}">{exact ? 'Would have matched: ' : 'Grouped here: '}{c.samples.map((m) => m.subject).join('; ')}</div>
          {/if}
          {#if c.groups.length}
            <div class="text-[12.5px] text-muted">From: {senders(c)}</div>
          {/if}
          {#if accounts.list.length > 1}
            <label class="flex flex-wrap items-center gap-2">
              <span class="w-[110px] text-[13px] font-semibold">Applies to</span>
              <select class="field flex-[0_1_260px] px-2.5" value={String(c.account_id ?? '')} onchange={(e) => change(c, { account_id: e.currentTarget.value ? Number(e.currentTarget.value) : null })}>
                <option value="">All mailboxes</option>
                {#each accounts.list as a (a.id)}
                  <option value={String(a.id)}>{a.label}</option>
                {/each}
              </select>
            </label>
          {/if}
          {#if c.refused}
            <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{c.refused}</div>
          {/if}
          {#if c.errors.length}
            <div class="border-t border-line-divider pt-2 text-[12.5px] font-semibold text-muted">Cannot be created as it is</div>
          {/if}
        </article>
      {/each}
    </div>
  {/if}
</div>
