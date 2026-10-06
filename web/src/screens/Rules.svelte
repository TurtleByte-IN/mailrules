<script lang="ts">
  import { router } from 'svelte-spa-router';
  import { ApiError } from '../lib/api/client';
  import * as rulesApi from '../lib/api/rules';
  import { confidence, day } from '../lib/format';
  import { accounts } from '../lib/state/accounts.svelte';
  import { edit, importFile, load, move, remove, rules, undoToday } from '../lib/state/rules.svelte';
  import { flash } from '../lib/state/toast.svelte';
  import MoreOptions from './rules/MoreOptions.svelte';
  import Rewrite from './rules/Rewrite.svelte';
  import { actionsText, condText, extrasText, kind, summary, treeWords } from './rules/text';

  // Add rules sends people back here with the rule they just saved selected.
  let selectedId = $state(Number(new URLSearchParams(router.querystring).get('id')));
  const sel = $derived(rules.list.find((r) => r.id === selectedId) ?? rules.list[0]);

  let dragId = $state(0);
  let testing = $state(false);
  // The last test: `ok` is false for one the daemon would not run, and `text` is empty when
  // there is no mailbox to test on.
  let result = $state<{ text: string; ok: boolean } | null>(null);
  // The slider's value while it is being dragged; saved on release.
  let thr = $state<number | null>(null);
  const sure = $derived(thr ?? sel?.min_confidence ?? null);
  // The field the daemon refused in the last edit, and why; shown beside that field.
  let refused = $state<{ path: string; message: string } | null>(null);
  const editorFields = ['name', 'intent', 'account_id', 'stack', 'model', 'min_confidence'];
  let picker: HTMLInputElement;
  let imported = $state<{ ok: boolean; text: string } | null>(null);

  const fail = (e: unknown) => flash((e as Error).message);
  const only = (r: rulesApi.Rule) => accounts.list.find((a) => String(a.id) === String(r.account_id))?.label;

  // The rule whose Rewrite with AI box is open.
  let rewriting = $state(0);

  function select(id: number) {
    selectedId = id;
    rewriting = 0;
    result = null;
    refused = null;
  }

  async function toggle(r: rulesApi.Rule, box: HTMLInputElement) {
    try {
      await edit(r.id, { enabled: !r.enabled });
      flash(r.name + (r.enabled ? ' paused' : ' turned on'));
    } catch (e) {
      box.checked = r.enabled;
      fail(e);
    }
  }

  async function save(patch: rulesApi.RulePatch) {
    refused = null;
    try {
      await edit(sel.id, patch);
    } catch (e) {
      if (e instanceof ApiError && e.status === 400 && e.path && editorFields.includes(e.path)) refused = { path: e.path, message: e.message };
      else fail(e);
    }
  }

  function rename(e: Event & { currentTarget: HTMLInputElement }) {
    const name = e.currentTarget.value.trim();
    if (name) save({ name });
    else e.currentTarget.value = sel.name;
  }

  async function test() {
    const account = sel.account_id ?? accounts.list[0]?.id;
    if (account === undefined) return void (result = { text: '', ok: false });
    testing = true;
    result = null;
    try {
      const t = await rulesApi.test({ account_id: Number(account), rule_ids: [sel.id], limit: 200 });
      result = {
        ok: true,
        text:
          `Matched ${t.matched} of your last ${t.tested} emails. ` +
          (t.model_calls
            ? `${t.model_calls} went to the decision model; ${t.results.filter((r) => r.review).length} below your threshold would go to review.`
            : 'Decided by conditions alone: no model calls.'),
      };
    } catch (e) {
      if (rulesApi.testRefused(e)) result = { text: e.message, ok: false };
      else fail(e);
    } finally {
      testing = false;
    }
  }

  async function undo() {
    const { id, name } = sel;
    try {
      const u = await undoToday(id);
      flash(
        (u.undone ? `Undid ${u.undone} ${u.undone === 1 ? 'action' : 'actions'}` : 'Nothing to undo') +
          ` from ${name} today` +
          (u.failed ? `; ${u.failed} could not be undone` : ''),
      );
    } catch (e) {
      fail(e);
    }
  }

  async function del() {
    const { id, name } = sel;
    try {
      await remove(id);
      result = null;
      flash(name + ' deleted');
    } catch (e) {
      fail(e);
    }
  }

  async function exportRules() {
    try {
      const a = document.createElement('a');
      a.href = URL.createObjectURL(await rulesApi.exportYaml());
      a.download = 'mailrules-rules.yaml';
      a.click();
      URL.revokeObjectURL(a.href);
    } catch (e) {
      fail(e);
    }
  }

  async function importRules(e: Event & { currentTarget: HTMLInputElement }) {
    const file = e.currentTarget.files?.[0];
    // Cleared so picking the same file again after fixing it fires this again.
    e.currentTarget.value = '';
    if (!file) return;
    try {
      const r = await importFile(file);
      imported = { ok: true, text: `Imported ${file.name}: ${r.created} added, ${r.updated} updated.` };
    } catch (err) {
      imported = { ok: false, text: (err as Error).message };
    }
  }
</script>

{#snippet problem(field: string)}
  {#if refused?.path === field}
    <p role="alert" class="text-[12.5px] text-trash">{refused.message}</p>
  {/if}
{/snippet}

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
    <button type="button" class="btn font-semibold" onclick={() => picker.click()}>Import rules</button>
    <button type="button" class="btn font-semibold" onclick={exportRules}>Export rules</button>
    <input bind:this={picker} type="file" accept=".yaml,.yml" hidden onchange={importRules} />
  </div>
  {#if imported?.ok}
    <div role="status" class="rounded bg-selected p-3 text-[13px]">{imported.text}</div>
  {:else if imported}
    <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] whitespace-pre-line text-warn">Nothing was imported. {imported.text}</div>
  {/if}
  {#if rules.error}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-3 rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">
      <span>Your rules could not be loaded. {rules.error}</span>
      <button type="button" class="btn font-semibold" onclick={load}>Retry</button>
    </div>
  {/if}
  {#if rules.loaded && !rules.list.length}
    <section class="card flex flex-col items-start gap-3 p-[18px]">
      <p>No rules yet.</p>
      <div class="flex flex-wrap gap-2">
        <a href="#/compose" class="btn-primary">Add rules</a>
        <button type="button" class="btn font-semibold" onclick={() => picker.click()}>Import rules</button>
      </div>
    </section>
  {:else if rules.list.length}
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
            ondragend={() => (dragId = 0)}
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
              <span class="text-[12.5px] text-muted">Then: {actionsText(r.actions)}{extrasText(r, only(r))} · {r.hits_week} this week{r.last_match_at ? ' · last match ' + day(r.last_match_at) : ''}</span>
            </button>
            <label class="flex items-center gap-2 text-[12.5px] text-secondary">
              <input type="checkbox" checked={r.enabled} onchange={(e) => toggle(r, e.currentTarget)} />On
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
          {@render problem('name')}
        </div>
        {#if sel.said}
          <div class="border-l-[3px] border-line-card py-1 pl-3">
            <div class="text-xs text-muted">You said</div>
            <div class="text-[13.5px] text-nav italic">“{sel.said}”</div>
          </div>
        {/if}
        {#if sel.intent}
          <div class="flex flex-col gap-1.5">
            <label for="rule-intent" class="text-[13px] font-semibold">When the email is about</label>
            <textarea id="rule-intent" rows="2" class="field h-auto resize-y py-2.5" value={sel.intent} onchange={(e) => save({ intent: e.currentTarget.value })}></textarea>
            {@render problem('intent')}
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
          <MoreOptions id="rule" value={sel} onchange={save} stackLabel="Stacks: also applies after another rule matched" />
          {@render problem('account_id')}
          {@render problem('stack')}
        </div>
        <div class="flex flex-wrap gap-3.5">
          <div class="flex flex-[1_1_160px] flex-col gap-1.5">
            <label for="rule-model" class="text-[13px] font-semibold">Model</label>
            <select id="rule-model" class="field px-2.5" value={sel.model} onchange={(e) => save({ model: e.currentTarget.value })}>
              <option value="">Default</option>
              <option value="jev">Jev</option>
              <option value="clef">Clef</option>
              <option value="anthropic">Claude Haiku 4.5</option>
            </select>
            {@render problem('model')}
          </div>
          <div class="flex flex-[1_1_160px] flex-col gap-1.5">
            <label for="rule-thr" class="text-[13px] font-semibold">Act when sure above {sure === null ? 'your default' : confidence(sure)}</label>
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
                save({ min_confidence: +e.currentTarget.value / 100 });
              }}
            />
            {@render problem('min_confidence')}
          </div>
        </div>
        <div class="flex flex-wrap items-center gap-2">
          <a href="#/compose?edit={sel.id}" class="btn-primary">Edit conditions</a>
          <button type="button" class="btn font-semibold" aria-expanded={rewriting === sel.id} onclick={() => (rewriting = rewriting === sel.id ? 0 : sel.id)}>Rewrite with AI</button>
          <button type="button" class="btn font-semibold" disabled={testing} onclick={test}>{testing ? 'Testing on last 200 emails…' : 'Test on last 200 emails'}</button>
          <button type="button" class="btn" onclick={undo}>Undo what it did today</button>
          <button type="button" class="min-h-10 rounded border-0 bg-transparent px-3.5 text-trash" onclick={del}>Delete rule</button>
        </div>
        {#if result?.ok}
          <div role="status" class="rounded bg-selected p-3 text-[13px]">{result.text}</div>
        {:else if result?.text}
          <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{result.text}</div>
        {:else if result}
          <div role="status" class="rounded bg-selected p-3 text-[13px]"><a href="#/accounts" class="font-semibold underline">Connect a mailbox</a> to test rules.</div>
        {/if}
      </aside>
    {/if}
  </div>
  {#if sel && rewriting === sel.id}
    {#key sel.id}
      <Rewrite rule={sel} onclose={() => (rewriting = 0)} />
    {/key}
  {/if}
  {/if}
</div>
