<script lang="ts">
  import { push } from 'svelte-spa-router';
  import { sample } from '../../lib/api/compose';
  import Waiting from '../../lib/components/Waiting.svelte';
  import { accounts } from '../../lib/state/accounts.svelte';
  import { compose, optimize, savable, saveAll, type Draft } from '../../lib/state/compose.svelte';
  import { kind } from '../rules/text';
  import RuleLines from './RuleLines.svelte';
  import TextBox from './TextBox.svelte';

  const keep = $derived(compose.drafts.filter(savable).length);
  // A refused save stays on its card until the card is changed or skipped.
  const change = (d: Draft, patch: Partial<Draft>) => Object.assign(d, patch, { refused: '', refusedName: false });

  let saving = $state(false);
  async function save() {
    saving = true;
    try {
      const added = await saveAll();
      if (added) push('/rules?id=' + added[0].id);
    } finally {
      saving = false;
    }
  }
</script>

<div class="flex flex-col gap-[18px]">
  <section class="card flex flex-col gap-3 p-[18px]">
    <TextBox id="compose" label="What should happen to your email?" placeholder="For example: bank statements go to Finance and mark them read. Archive LinkedIn profile-view emails. Trash cold sales pitches from people I've never emailed." bind:value={compose.text}>
      {#snippet actions()}
        <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={compose.busy} onclick={optimize}>{compose.busy ? 'Tidying up your rules…' : 'Turn into rules'}</button>
        <button type="button" class="min-h-11 border-0 bg-transparent px-3.5 font-semibold" onclick={() => (compose.text = sample)}>Use an example</button>
      {/snippet}
    </TextBox>
  </section>

  {#if compose.busy}
    <Waiting text="Asking the AI model to draft your rules" />
  {/if}
  {#if compose.needsModel}
    <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{compose.needsModel} <a href="#/settings" class="font-semibold underline">Open Settings</a></div>
  {/if}
  {#if compose.unparsed.length}
    <p role="status" class="text-[13px] text-secondary">Not turned into a rule: {compose.unparsed.map((u) => `“${u}”`).join('; ')}</p>
  {/if}
  {#if compose.drafts.length}
    <div class="flex flex-col gap-3.5">
      <div class="flex flex-wrap items-center justify-between gap-2.5">
        <h2>{compose.drafts.length}{compose.drafts.length === 1 ? ' rule found' : ' rules found'}. Check them before saving.</h2>
        <div class="flex gap-2">
          <button type="button" class="btn" disabled={saving} onclick={() => (compose.drafts = [])}>Discard</button>
          <button type="button" class="btn-primary" disabled={saving} onclick={save}>{saving ? 'Saving…' : `Save ${keep}${keep === 1 ? ' rule' : ' rules'}`}</button>
        </div>
      </div>
      {#each compose.drafts as d}
        {@const k = kind(d)}
        <article class="flex flex-col gap-3 rounded-md border border-line-input bg-surface p-[18px] {savable(d) ? '' : 'border-dashed opacity-60'}">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div class="flex flex-wrap items-center gap-2"><!-- Editable: a name the daemon refuses (taken, or twice in one save) can only be fixed here. -->
              <input class="field h-9 w-[220px] max-w-full text-base font-semibold" aria-label="Rule name" aria-invalid={d.refusedName || undefined} value={d.name} oninput={(e) => change(d, { name: e.currentTarget.value })} /><span class={k.chip}>{k.label}</span></div>
            <!-- `tested` is 0 when nothing was tested: a draft in error, one by meaning with no decision model, or no mailbox to test on. -->
            {#if !d.errors.length && d.tested}
              <span class="text-[12.5px] text-secondary">Matches {d.match_count} of your last {d.tested === 1 ? 'email' : `${d.tested.toLocaleString()} emails`}{d.intent ? '' : ' · no model needed'}</span>
            {/if}
          </div>
          <div class="flex flex-wrap gap-3.5">
            <div class="min-w-0 flex-[1_1_240px] border-l-[3px] border-line-card py-0.5 pl-3">
              <div class="text-xs text-muted">You said</div>
              <div class="text-[13.5px] text-nav italic">“{d.said}”</div>
            </div>
            <div class="min-w-0 flex-[2_1_320px]"><RuleLines rule={d} /></div>
          </div>
          {#each d.conflicts as c}
            <div class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{c.note}</div>
          {/each}
          {#each d.errors as e}
            <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{e.message}</div>
          {/each}
          {#if d.question}
            <div class="text-[13px]"><span class="font-semibold">{d.question}</span> <span class="text-secondary">Say which in your own words above, then turn it into rules again.</span></div>
          {/if}
          {#if d.samples.length}
            <div class="text-[12.5px] text-muted">Would have matched: {d.samples.map((m) => m.subject).join('; ')}</div>
          {/if}
          {#if accounts.list.length > 1}
            <label class="flex flex-wrap items-center gap-2">
              <span class="w-[110px] text-[13px] font-semibold">Applies to</span>
              <select class="field flex-[0_1_260px] px-2.5" value={String(d.account_id ?? '')} onchange={(e) => change(d, { account_id: e.currentTarget.value ? Number(e.currentTarget.value) : null })}>
                <option value="">All mailboxes</option>
                {#each accounts.list as a (a.id)}
                  <option value={String(a.id)}>{a.label}</option>
                {/each}
              </select>
            </label>
          {/if}
          {#if d.refused}
            <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{d.refused}</div>
          {/if}
          <div class="flex items-center gap-2 border-t border-line-divider pt-2">
            {#if d.errors.length}
              <span class="text-[12.5px] font-semibold text-muted">Cannot be saved as it is</span>
            {:else}
              <span class="text-[12.5px] font-semibold {d.rejected ? 'text-muted' : ''}">{d.rejected ? 'Skipped' : 'Will be saved'}</span>
              <button type="button" class="btn ml-auto min-h-9 px-3 text-[13px]" onclick={() => change(d, { rejected: !d.rejected })}>{d.rejected ? 'Include' : 'Skip'}</button>
            {/if}
          </div>
        </article>
      {/each}
    </div>
  {/if}
</div>
