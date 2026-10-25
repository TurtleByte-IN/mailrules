<script lang="ts">
  import { onMount } from 'svelte';
  import { needsSettings } from '../../lib/api/client';
  import { recompose, type Draft, type MatchShift } from '../../lib/api/compose';
  import type { Rule, RulePatch } from '../../lib/api/rules';
  import Waiting from '../../lib/components/Waiting.svelte';
  import { confidence } from '../../lib/format';
  import { edit } from '../../lib/state/rules.svelte';
  import { flash } from '../../lib/state/toast.svelte';
  import RuleLines from '../compose/RuleLines.svelte';
  import TextBox from '../compose/TextBox.svelte';
  import { actionsText, kind, modelLabel } from './text';

  /**
   * The saved rule to rewrite. The parent re-creates this when another rule is selected.
   * `improve` asks at once with no text: Improve rule, from how the rule behaved.
   */
  let { rule, improve = false, onclose }: { rule: Rule; improve?: boolean; onclose: () => void } = $props();

  let panel: HTMLElement;
  // The rule list can be long; bring the box into view.
  onMount(() => {
    panel.scrollIntoView?.({ block: 'nearest' });
    if (improve) void ask('');
  });

  let text = $state('');
  let busy = $state(false);
  let draft = $state<Draft | null>(null);
  let changes = $state<string[]>([]);
  let matching = $state<MatchShift[]>([]);
  // A new action or threshold is only applied when the owner ticks it.
  let takeActions = $state(false);
  let takeThreshold = $state(false);
  // The daemon's own sentence when it has no model to compose with.
  let needsModel = $state('');
  // The daemon's sentence when it refused to save the draft.
  let refused = $state('');

  const sorted = (_: string, v: unknown) => (v && typeof v === 'object' && !Array.isArray(v) ? Object.fromEntries(Object.entries(v).sort(([a], [b]) => (a < b ? -1 : 1))) : v);
  const json = (v: unknown) => JSON.stringify(v, sorted);
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
  const actionsChanged = $derived(draft !== null && json(draft.actions) !== json(rule.actions));
  const thresholdChanged = $derived(draft !== null && (draft.min_confidence ?? null) !== (rule.min_confidence ?? null));
  /** The draft as Update rule saves it: the rule's own actions and threshold unless their change is ticked. */
  const proposed = $derived(
    draft && {
      ...draft,
      actions: actionsChanged && !takeActions ? rule.actions : draft.actions,
      new_folders: actionsChanged && !takeActions ? [] : draft.new_folders,
      min_confidence: thresholdChanged && !takeThreshold ? rule.min_confidence : draft.min_confidence,
    },
  );
  const same = $derived(draft !== null && json(fieldsOf(draft)) === json(fieldsOf(rule)));
  const unchanged = $derived(proposed !== null && json(fieldsOf(proposed)) === json(fieldsOf(rule)));
  const threshold = (c: number | null) => (c === null ? 'your default' : confidence(c));

  function rewrite() {
    if (!text.trim()) return flash('Type or dictate what should change first');
    return ask(text.trim());
  }

  /** Asks for a draft: from `words`, or with none, from how the rule behaved. */
  async function ask(words: string) {
    busy = true;
    needsModel = '';
    try {
      const res = await recompose(rule.id, words);
      draft = res.rule;
      changes = res.changes;
      matching = res.matching;
      takeActions = takeThreshold = false;
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
    if (!draft || !proposed) return;
    const patch: RulePatch = { ...fieldsOf(proposed), said: draft.said };
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
      Model: {modelLabel(r.model)}{r.intent ? ' · Act when sure above ' + threshold(r.min_confidence) : ''}
    </div>
  </section>
{/snippet}

{#snippet shifts(title: string, change: MatchShift['change'])}
  {@const rows = matching.filter((m) => m.change === change)}
  {#if rows.length}
    <section aria-label={title} class="flex flex-col gap-1.5">
      <div class="text-[13px] font-semibold">{title} ({rows.length})</div>
      <ul class="m-0 flex list-none flex-col gap-1 p-0 text-[13px]">
        {#each rows as m}
          {@const why = change === 'starts' ? m.after : m.before}
          <li class="min-w-0">
            <span class="font-medium">{m.after.subject}</span> <span class="text-secondary">· {m.after.from}</span>
            {#if why.reason}<div class="text-[12.5px] text-muted">{why.reason}{why.confidence ? ' (' + confidence(why.confidence) + ')' : ''}</div>{/if}
          </li>
        {/each}
      </ul>
    </section>
  {/if}
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

  {#if draft && proposed}
    <div class="flex flex-wrap gap-3.5 border-t border-line-divider pt-3.5">
      {@render column('Current rule', rule)}
      {@render column('New draft', proposed)}
    </div>
    {#if changes.length}
      <section aria-label="What changed" class="flex flex-col gap-1.5">
        <div class="text-[13px] font-semibold">What changed</div>
        <ul class="m-0 list-disc pl-5 text-[13px]">
          {#each changes as c}<li>{c}</li>{/each}
        </ul>
      </section>
    {/if}
    {#if actionsChanged || thresholdChanged}
      <section aria-label="Also suggested" class="flex flex-col gap-2 rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px]">
        <div class="font-semibold text-warn">Also suggested, not applied unless you tick it</div>
        {#if actionsChanged}
          <label class="flex items-start gap-2">
            <input type="checkbox" bind:checked={takeActions} />
            <span>Change what it does: {actionsText(rule.actions)} → {actionsText(draft.actions)}</span>
          </label>
        {/if}
        {#if thresholdChanged}
          <label class="flex items-start gap-2">
            <input type="checkbox" bind:checked={takeThreshold} />
            <span>Change act when sure above: {threshold(rule.min_confidence)} → {threshold(draft.min_confidence)}</span>
          </label>
        {/if}
      </section>
    {/if}
    {@render shifts('Starts matching', 'starts')}
    {@render shifts('Stops matching', 'stops')}
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
      <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={!!draft.errors.length || unchanged} onclick={approve}>Update rule</button>
      <button type="button" class="btn min-h-11 px-4" onclick={() => (draft = null)}>Discard</button>
      {#if draft.errors.length}
        <span class="text-[12.5px] font-semibold text-muted">Cannot be saved as it is</span>
      {/if}
    </div>
  {/if}
</section>
