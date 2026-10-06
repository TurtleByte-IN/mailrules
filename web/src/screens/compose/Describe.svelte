<script lang="ts">
  import { onDestroy } from 'svelte';
  import { push } from 'svelte-spa-router';
  import { sample } from '../../lib/api/compose';
  import { leaves } from '../../lib/api/rules';
  import { answer, compose, optimize, saveAll } from '../../lib/state/compose.svelte';
  import { actionsText, condText, kind, treeWords } from '../rules/text';
  import { listen, supported } from './dictation';

  const canDictate = supported();
  // Set while listening.
  let stop = $state<(() => void) | null>(null);
  let micBlocked = $state(false);

  function toggleMic() {
    if (stop) return stop();
    const before = compose.text.trim();
    micBlocked = false;
    stop = listen(
      (heard) => (compose.text = ((before ? before + ' ' : '') + heard).slice(0, 4000)),
      (error) => {
        stop = null;
        micBlocked = error === 'not-allowed' || error === 'service-not-allowed';
      },
    );
  }
  onDestroy(() => stop?.());

  const keep = $derived(compose.drafts.filter((d) => !d.rejected).length);

  async function save() {
    const added = await saveAll();
    if (added) push('/rules?id=' + added[0].id);
  }
</script>

<div class="flex flex-col gap-[18px]">
  <section class="card flex flex-col gap-3 p-[18px]">
    <label for="compose" class="text-[13px] font-semibold">What should happen to your email?</label>
    <textarea
      id="compose"
      rows="5"
      maxlength="4000"
      bind:value={compose.text}
      placeholder="For example: bank statements go to Finance and mark them read. Archive LinkedIn profile-view emails. Trash cold sales pitches from people I've never emailed."
      class="resize-y rounded-md border border-line-input bg-surface p-3.5 text-[15px] leading-normal text-ink"
    ></textarea>
    <div class="flex flex-wrap items-center gap-2.5">
      <button type="button" aria-pressed={!!stop} disabled={!canDictate} class="btn min-h-11 px-4 font-semibold {stop ? 'border-trash bg-trash-bg text-trash' : ''}" onclick={toggleMic}>
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="3" width="6" height="11" rx="3" /><path d="M5 11a7 7 0 0 0 14 0M12 18v3" /></svg>
        {stop ? 'Listening… tap to stop' : 'Dictate'}
      </button>
      <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={compose.busy} onclick={optimize}>{compose.busy ? 'Tidying up your rules…' : 'Turn into rules'}</button>
      <button type="button" class="min-h-11 border-0 bg-transparent px-3.5 font-semibold" onclick={() => (compose.text = sample)}>Use an example</button>
      <span class="ml-auto text-[12.5px] text-muted">{compose.text.length} / 4000</span>
    </div>
    {#if !canDictate}
      <p role="status" class="text-[12.5px] text-muted">This browser has no speech recognition, so dictation is off. Type your rules instead.</p>
    {:else if micBlocked}
      <p role="status" class="text-[12.5px] text-muted">The microphone is blocked for this page. Allow it in the browser, or type your rules instead.</p>
    {/if}
  </section>

  {#if compose.drafts.length}
    <div class="flex flex-col gap-3.5">
      <div class="flex flex-wrap items-center justify-between gap-2.5">
        <h2>{compose.drafts.length}{compose.drafts.length === 1 ? ' rule found' : ' rules found'}. Check them before saving.</h2>
        <div class="flex gap-2">
          <button type="button" class="btn" onclick={() => (compose.drafts = [])}>Discard</button>
          <button type="button" class="btn-primary" onclick={save}>Save {keep}{keep === 1 ? ' rule' : ' rules'}</button>
        </div>
      </div>
      {#each compose.drafts as d}
        {@const k = kind(d)}
        <article class="flex flex-col gap-3 rounded-md border border-line-input bg-surface p-[18px] {d.rejected ? 'border-dashed opacity-60' : ''}">
          <div class="flex flex-wrap items-center justify-between gap-2">
            <div class="flex flex-wrap items-center gap-2"><span class="text-base font-semibold">{d.name}</span><span class={k.chip}>{k.label}</span></div>
            <span class="text-[12.5px] text-secondary">Matches {d.match_count} of your last 200 emails{d.intent ? '' : ' · no model needed'}</span>
          </div>
          <div class="flex flex-wrap gap-3.5">
            <div class="min-w-0 flex-[1_1_240px] border-l-[3px] border-line-card py-0.5 pl-3">
              <div class="text-xs text-muted">You said</div>
              <div class="text-[13.5px] text-nav italic">“{d.said}”</div>
            </div>
            <div class="flex min-w-0 flex-[2_1_320px] flex-col gap-1.5 text-[13.5px]">
              {#if d.intent}
                <div><span class="text-muted">When</span> {d.intent}</div>
              {/if}
              {#if leaves(d.conditions).length}
                <div class="flex flex-wrap items-center gap-1.5">
                  <span class="text-muted">Match</span>
                  {#each leaves(d.conditions) as c}
                    <span class="rounded bg-neutral px-2 py-[3px] font-mono text-[12.5px] text-ink-soft">{condText(c)}</span>
                  {/each}
                </div>
              {/if}
              {#if treeWords(d.exceptions)}
                <div><span class="text-muted">Unless</span> {treeWords(d.exceptions)}</div>
              {/if}
              <div>
                <span class="text-muted">Then</span>
                {actionsText(d.actions)}
                {#each d.new_folders as f}
                  <span class="chip chip-both ml-2 font-normal">new folder: {f}</span>
                {/each}
              </div>
            </div>
          </div>
          {#each d.conflicts as c}
            <div class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{c.note}</div>
          {/each}
          {#if d.question}
            <div class="flex flex-wrap items-center gap-2 text-[13px]">
              <span class="font-semibold">{d.question}</span>
              {#each d.options as o}
                <button type="button" aria-pressed={d.answer === o} class="min-h-9 rounded border px-3 text-[13px] {d.answer === o ? 'border-ink bg-ink text-surface' : 'border-line-input bg-surface text-ink-soft'}" onclick={() => answer(d, o)}>{o}</button>
              {/each}
            </div>
          {/if}
          {#if d.samples.length}
            <div class="text-[12.5px] text-muted">Would have matched: {d.samples.join('; ')}</div>
          {/if}
          <div class="flex items-center gap-2 border-t border-line-divider pt-2">
            <span class="text-[12.5px] font-semibold {d.rejected ? 'text-muted' : ''}">{d.rejected ? 'Skipped' : 'Will be saved'}</span>
            <button type="button" class="btn ml-auto min-h-9 px-3 text-[13px]" onclick={() => (d.rejected = !d.rejected)}>{d.rejected ? 'Include' : 'Skip'}</button>
          </div>
        </article>
      {/each}
    </div>
  {/if}
</div>
