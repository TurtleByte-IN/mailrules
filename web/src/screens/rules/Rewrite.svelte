<script lang="ts">
  import { onMount } from 'svelte';
  import { needsSettings } from '../../lib/api/client';
  import { recompose, type Draft } from '../../lib/api/compose';
  import type { Rule, RulePatch } from '../../lib/api/rules';
  import Waiting from '../../lib/components/Waiting.svelte';
  import { confidence } from '../../lib/format';
  import { edit } from '../../lib/state/rules.svelte';
  import { flash } from '../../lib/state/toast.svelte';
  import RuleLines from '../compose/RuleLines.svelte';
  import TextBox from '../compose/TextBox.svelte';
  import { kind } from './text';

  /** The saved rule to rewrite. The parent re-creates this when another rule is selected. */
  let { rule, onclose }: { rule: Rule; onclose: () => void } = $props();

  let panel: HTMLElement;
  // The rule list can be long; bring the box into view.
  onMount(() => panel.scrollIntoView?.({ block: 'nearest' }));

  let text = $state('');
  let busy = $state(false);
  let draft = $state<Draft | null>(null);
  // The daemon's own sentence when it has no model to compose with.
  let needsModel = $state('');
  // The daemon's sentence when it refused to save the draft.
  let refused = $state('');

  const models: Record<string, string> = { '': 'Default', jev: 'Jev', clef: 'Clef', anthropic: 'Claude Haiku 4.5' };
  const sorted = (_: string, v: unknown) => (v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v);
  /** What the daemon would store for a draft: the same fields PATCH takes, so a draft and a rule compare directly. */
  const fieldsOf = (r: Pick<Rule, 'name' | 'intent' | 'conditions' | 'exceptions' | 'actions' | 'account_id' | 'stack' | 'model' | 'min_confidence'> | Draft) => ({
    name: r.name,
    intent: r.intent ?? '',
    conditions: r.conditions,
    exceptions: r.exceptions,
    actions: r.actions,
    account_id: r.account_id,
    stack: r.stack,
    model: r.model,
    min_confidence: r.min_confidence,
  });
  const same = $derived(draft !== null && JSON.stringify(fieldsOf(draft), sorted) === JSON.stringify(fieldsOf(rule), sorted));

  async function rewrite() {
    if (!text.trim()) return flash('Type or dictate what should change first');
    busy = true;
    needsModel = '';
    try {
      draft = await recompose(rule.id, text.trim());
      refused = '';
    } catch (e) {
      if (needsSettings(e)) needsModel = e.message;
      else flash((e as Error).message);
    } finally {
      busy = false;
    }
  }

  /** Updates the rule in place: same id, position, on/off state and history. */
  async function approve() {
    if (!draft) return;
    const patch: RulePatch = { ...fieldsOf(draft), said: draft.said };
    try {
      await edit(rule.id, patch);
      flash(draft.name + ' updated');
      onclose();
    } catch (e) {
      refused = (e as Error).message;
    }
  }
</script>

{#snippet column(title: string, r: { name: string; intent?: string | null; conditions: Rule['conditions']; exceptions: Rule['exceptions']; actions: Rule['actions']; model: string; min_confidence: number | null; said: string; new_folders?: string[] })}
  {@const k = kind(r)}
  <section aria-label={title} class="flex min-w-0 flex-[1_1_320px] flex-col gap-3 rounded-md border border-line-input bg-surface p-4">
    <div class="text-xs font-medium tracking-[0.06em] text-muted uppercase">{title}</div>
    <div class="flex flex-wrap items-center gap-2"><span class="text-base font-semibold">{r.name}</span><span class={k.chip}>{k.label}</span></div>
    {#if r.said}
      <div class="border-l-[3px] border-line-card py-0.5 pl-3">
        <div class="text-xs text-muted">You said</div>
        <div class="text-[13.5px] text-nav italic">“{r.said}”</div>
      </div>
    {/if}
    <RuleLines rule={r} />
    <div class="text-[13px] text-secondary">
      Model: {models[r.model] ?? r.model}{r.intent ? ' · Act when sure above ' + (r.min_confidence === null ? 'your default' : confidence(r.min_confidence)) : ''}
    </div>
  </section>
{/snippet}

<section bind:this={panel} aria-label="Rewrite with AI" class="card flex flex-col gap-3 p-[18px]">
  <TextBox id="rewrite" label="What should change about this rule?" placeholder="For example: also skip anything from my bank." bind:value={text}>
    {#snippet actions()}
      <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={busy} onclick={rewrite}>{busy ? 'Tidying up your rules…' : 'Rewrite rule'}</button>
      <button type="button" class="min-h-11 border-0 bg-transparent px-3.5 text-secondary" onclick={onclose}>Cancel</button>
    {/snippet}
  </TextBox>

  {#if busy}
    <Waiting text="Asking the AI model to rewrite this rule" />
  {/if}
  {#if needsModel}
    <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{needsModel} <a href="#/settings" class="font-semibold underline">Open Settings</a></div>
  {/if}

  {#if draft}
    <div class="flex flex-wrap gap-3.5 border-t border-line-divider pt-3.5">
      {@render column('Current rule', rule)}
      {@render column('New draft', draft)}
    </div>
    {#each draft.conflicts as c}
      <div class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{c.note}</div>
    {/each}
    {#each draft.errors as e}
      <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{e.message}</div>
    {/each}
    {#if draft.question}
      <div class="text-[13px]"><span class="font-semibold">{draft.question}</span> <span class="text-secondary">Answer in your own words above, then rewrite again.</span></div>
    {/if}
    {#if same}
      <p role="status" class="text-[13px] text-secondary">This is the same as the current rule.</p>
    {/if}
    {#if draft.samples.length}
      <div class="text-[12.5px] text-muted">Would have matched: {draft.samples.map((m) => m.subject).join('; ')}</div>
    {/if}
    {#if refused}
      <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{refused}</div>
    {/if}
    <div class="flex flex-wrap items-center gap-2 border-t border-line-divider pt-3">
      <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={!!draft.errors.length || same} onclick={approve}>Update rule</button>
      <button type="button" class="btn min-h-11 px-4" onclick={() => (draft = null)}>Discard</button>
      {#if draft.errors.length}
        <span class="text-[12.5px] font-semibold text-muted">Cannot be saved as it is</span>
      {/if}
    </div>
  {/if}
</section>
