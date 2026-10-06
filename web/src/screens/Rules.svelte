<script lang="ts">
  import { router } from 'svelte-spa-router';
  import * as rulesApi from '../lib/api/rules';
  import { confidence } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { edit, move, remove, rules } from '../lib/state/rules.svelte';
  import { flash } from '../lib/state/toast.svelte';
  import MoreOptions from './rules/MoreOptions.svelte';
  import { actionsText, condText, extrasText, kind, summary, treeWords } from './rules/text';

  // Add rules sends people back here with the rule they just saved selected.
  let selectedId = $state(new URLSearchParams(router.querystring).get('id') ?? '');
  const sel = $derived(rules.list.find((r) => r.id === selectedId) ?? rules.list[0]);

  let dragId = $state('');
  let testing = $state(false);
  let result = $state('');
  // The slider's value while it is being dragged; saved on release.
  let thr = $state<number | null>(null);

  const only = (r: rulesApi.Rule) => accounts.list.find((a) => a.id === r.account_id)?.email;

  function select(id: string) {
    selectedId = id;
    result = '';
  }

  async function toggle(r: rulesApi.Rule) {
    await edit(r.id, { enabled: !r.enabled });
    flash(r.name + (r.enabled ? ' paused' : ' turned on'));
  }

  function rename(e: Event & { currentTarget: HTMLInputElement }) {
    const name = e.currentTarget.value.trim();
    if (name) edit(sel.id, { name });
    else e.currentTarget.value = sel.name;
  }

  async function test() {
    testing = true;
    result = '';
    const t = await rulesApi.test(sel);
    testing = false;
    result =
      `Matched ${t.matched} of your last ${t.limit} emails. ` +
      (t.avg_confidence === null
        ? 'Decided by conditions alone: no model calls.'
        : `Average confidence ${confidence(t.avg_confidence)}; ${t.to_review} below your threshold would go to review.`);
  }

  async function del() {
    const { id, name } = sel;
    await remove(id);
    result = '';
    flash(name + ' deleted');
  }
</script>

<div class="flex flex-col gap-[18px]">
  <header class="flex flex-wrap items-end justify-between gap-4">
    <div>
      <h1>Rules</h1>
      <p class="mt-1 text-secondary">Checked top to bottom; the first confident match wins.</p>
    </div>
    <a href="#/compose" class="btn-primary">
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
      Add rules
    </a>
  </header>
  <div class="-mt-1.5 flex flex-wrap gap-2">
    <a href="#/compose?mode=build" class="btn font-semibold">New condition rule</a>
  </div>
  <div class="flex flex-wrap items-start gap-[18px]">
    <section aria-label="Rule list" class="card min-w-0 flex-[3_1_560px] overflow-hidden">
      <ul class="m-0 list-none p-0">
        {#each rules.list as r, i (r.id)}
          {@const k = kind(r)}
          <li
            draggable="true"
            ondragstart={() => (dragId = r.id)}
            ondragover={(e) => e.preventDefault()}
            ondrop={() => move(dragId, i)}
            ondragend={() => (dragId = '')}
            class="flex flex-wrap items-center gap-x-3.5 gap-y-2.5 border-t border-l-[3px] border-line-divider px-4 py-3.5 {r.id === sel.id ? 'border-l-signal bg-selected-row' : 'border-l-transparent'} {r.enabled ? '' : 'opacity-55'}"
          >
            <div class="flex flex-col gap-0.5">
              <button type="button" class="grid h-[22px] w-7 place-items-center rounded-sm border border-line-card bg-surface p-0 text-secondary" disabled={i === 0} aria-label="Move {r.name} up" onclick={() => move(r.id, i - 1)}>
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 15 6-6 6 6" /></svg>
              </button>
              <button type="button" class="grid h-[22px] w-7 place-items-center rounded-sm border border-line-card bg-surface p-0 text-secondary" disabled={i === rules.list.length - 1} aria-label="Move {r.name} down" onclick={() => move(r.id, i + 1)}>
                <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
              </button>
            </div>
            <span class="w-[18px] font-mono text-[12.5px] text-muted">{i + 1}</span>
            <button type="button" class="flex min-w-0 flex-[1_1_260px] flex-col gap-0.5 border-0 bg-transparent p-0 text-left" aria-pressed={r.id === sel.id} onclick={() => select(r.id)}>
              <span class="flex flex-wrap items-center gap-2"><span class="font-semibold">{r.name}</span><span class={k.chip}>{k.label}</span></span>
              <span class="text-[12.5px] text-secondary">{summary(r)}</span>
              <span class="text-[12.5px] text-muted">Then: {actionsText(r.actions)}{extrasText(r, only(r))} · {r.hits} this week</span>
            </button>
            <label class="flex items-center gap-2 text-[12.5px] text-secondary">
              <input type="checkbox" checked={r.enabled} onchange={() => toggle(r)} />On
            </label>
          </li>
        {/each}
      </ul>
    </section>

    {#if sel}
      <aside aria-label="Edit rule" class="card flex min-w-0 flex-[2_1_360px] flex-col gap-3.5 p-[18px]">
        <div class="text-xs font-medium tracking-[0.06em] text-muted uppercase">Edit rule</div>
        <div class="flex flex-col gap-1.5">
          <label for="rule-name" class="text-[13px] font-semibold">Name</label>
          <input id="rule-name" class="field" value={sel.name} onchange={rename} />
        </div>
        <div class="border-l-[3px] border-line-card py-1 pl-3">
          <div class="text-xs text-muted">You said</div>
          <div class="text-[13.5px] text-nav italic">“{sel.said}”</div>
        </div>
        {#if sel.intent}
          <div class="flex flex-col gap-1.5">
            <label for="rule-intent" class="text-[13px] font-semibold">When the email is about</label>
            <textarea id="rule-intent" rows="2" class="field h-auto resize-y py-2.5" value={sel.intent} onchange={(e) => edit(sel.id, { intent: e.currentTarget.value })}></textarea>
          </div>
        {/if}
        {#if rulesApi.leaves(sel.conditions).length}
          <div class="flex flex-col gap-1.5">
            <div class="text-[13px] font-semibold">Conditions</div>
            <div class="flex flex-wrap gap-1.5">
              {#each rulesApi.leaves(sel.conditions) as c}
                <span class="rounded bg-neutral px-2 py-1 font-mono text-[12.5px] text-ink-soft">{condText(c)}</span>
              {/each}
            </div>
          </div>
        {/if}
        {#if treeWords(sel.exceptions)}
          <div class="text-[13px]"><span class="font-semibold">Unless</span> <span class="text-nav">{treeWords(sel.exceptions)}</span></div>
        {/if}
        <div class="text-[13px]"><span class="font-semibold">Then</span> <span class="text-nav">{actionsText(sel.actions)}</span></div>
        <div class="flex flex-col gap-2.5 rounded-md border border-line-divider bg-selected-row p-3">
          <MoreOptions id="rule" value={sel} onchange={(p) => edit(sel.id, p)} stackLabel="Stacks: also applies after another rule matched" draftLabel="Draft a reply (never sent automatically)" />
        </div>
        <div class="flex flex-wrap gap-3.5">
          <div class="flex flex-[1_1_160px] flex-col gap-1.5">
            <label for="rule-model" class="text-[13px] font-semibold">Model</label>
            <select id="rule-model" class="field px-2.5" value={sel.model ?? ''} onchange={(e) => edit(sel.id, { model: e.currentTarget.value || null })}>
              <option value="">Default</option>
              <option value="jev">Jev</option>
              <option value="clef">Clef</option>
              <option value="anthropic">Claude Haiku 4.5</option>
            </select>
          </div>
          <div class="flex flex-[1_1_160px] flex-col gap-1.5">
            <label for="rule-thr" class="text-[13px] font-semibold">Act when sure above {confidence(thr ?? sel.min_confidence ?? 0.75)}</label>
            <input
              id="rule-thr"
              type="range"
              min="50"
              max="99"
              class="h-10 accent-ink"
              disabled={!sel.intent}
              value={Math.round((sel.min_confidence ?? 0.75) * 100)}
              oninput={(e) => (thr = +e.currentTarget.value / 100)}
              onchange={(e) => {
                thr = null;
                edit(sel.id, { min_confidence: +e.currentTarget.value / 100 });
              }}
            />
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <a href="#/compose?edit={sel.id}" class="btn-primary">Edit conditions</a>
          <button type="button" class="btn font-semibold" disabled={testing} onclick={test}>{testing ? 'Testing on last 200 emails…' : 'Test on last 200 emails'}</button>
          <button type="button" class="min-h-10 rounded border-0 bg-transparent px-3.5 text-trash" onclick={del}>Delete rule</button>
        </div>
        {#if result}
          <div role="status" class="rounded bg-selected p-3 text-[13px]">{result}</div>
        {/if}
      </aside>
    {/if}
  </div>
</div>
