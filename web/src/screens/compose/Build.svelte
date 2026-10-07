<script lang="ts">
  import { push } from 'svelte-spa-router';
  import { ApiError } from '../../lib/api/client';
  import * as rulesApi from '../../lib/api/rules';
  import TestRunner from '../../lib/components/TestRunner.svelte';
  import { accounts } from '../../lib/state/accounts.svelte';
  import { add, edit, rules } from '../../lib/state/rules.svelte';
  import { flash } from '../../lib/state/toast.svelte';
  import MoreOptions from '../rules/MoreOptions.svelte';
  import Threshold from '../rules/Threshold.svelte';
  import { condText, fields } from '../rules/text';
  import { emptyBuilder, english, filled, fromRule, opsFor, refusedPart, toCondition, toRule, type Builder } from './builder';

  /** The saved rule being edited; none for a new rule. The parent re-creates this component when it changes. */
  let { rule }: { rule?: rulesApi.Rule } = $props();

  const start = () => (rule ? fromRule(rule) : emptyBuilder());
  let b = $state(start());
  // A test result is shown only for the form it was run on. `ok` is false for a test the daemon
  // would not run; `text` is empty when there is no mailbox to test on.
  let tested = $state({ form: '', text: '', ok: true });
  const form = $derived(JSON.stringify(toRule(b)));

  // The save the daemon last refused: the part of the form its path names, and that part as
  // it was, so the message goes once the part is edited.
  let refused = $state<{ part: string; leaf?: string; message: string; was: string } | null>(null);
  const snapshot = (part: string) => JSON.stringify(part.startsWith('row') ? b.rows[+part.slice(3)] : b[part as keyof Builder]);
  const problem = $derived(refused && refused.was === snapshot(refused.part) ? refused : null);
  const bad = (part: string, leaf?: string) => (problem?.part === part && problem.leaf === leaf ? { 'aria-invalid': true, 'aria-describedby': 'b-problem' } : {});
  let more = $state(false);

  const empty = $derived(!filled(b).length && !b.intent.trim());
  // Folders other rules already move mail to. The mailbox's own folder list arrives with accounts.
  const folders = $derived([...new Set(rules.list.flatMap((r) => r.actions.flatMap((a) => a.folder ?? [])))]);

  function setField(i: number, field: string) {
    b.rows[i] = { field, op: opsFor(fields[field].type)[0][0], value: '' };
  }

  async function test(limit: number, onProgress: (p: rulesApi.TestProgress) => void) {
    if (empty) return flash('Add a condition with a value, or say what the email is about');
    const account = b.account_id ?? accounts.list[0]?.id;
    if (account === undefined) return void (tested = { form, text: '', ok: false });
    const r = toRule(b);
    try {
      const t = await rulesApi.test({ account_id: Number(account), rules: [{ ...r, enabled: true }], limit }, onProgress);
      // A draft's rows carry its name and no rule id.
      const latest = t.results.filter((x) => x.rule_id === null && x.rule_name === r.name).sort((x, y) => (y.received_at ?? 0) - (x.received_at ?? 0))[0];
      tested = {
        form,
        ok: true,
        text:
          `Matched ${t.matched} of your last ${t.tested} emails, ` +
          (t.model_calls ? `with ${t.model_calls} sent to the decision model.` : 'all decided by conditions with no model calls.') +
          (latest ? ` Latest: mail from ${latest.from}.` : ''),
      };
    } catch (e) {
      if (rulesApi.limitRefused(e)) throw e; // shown beside the number
      if (rulesApi.testRefused(e)) tested = { form, text: e.message, ok: false };
      else flash((e as Error).message);
    }
  }

  let saving = $state(false);
  async function save() {
    if (empty) return flash('Add a condition with a value, or say what the email is about');
    if (b.action === 'move' && !b.folder.trim()) return flash('Choose a folder to move these emails to');
    const r = toRule(b);
    const id = b.editingId;
    saving = true;
    try {
      const saved = id ? await edit(id, r) : (await add([{ ...r, said: 'Built with conditions', enabled: true }]))[0];
      flash(r.name + (id ? ' updated' : ' saved and live'));
      push('/rules?id=' + saved.id);
    } catch (e) {
      const at = e instanceof ApiError && e.path ? refusedPart(b, e.path) : undefined;
      if (!at) return flash((e as Error).message);
      refused = { ...at, message: (e as Error).message, was: snapshot(at.part) };
      if (at.part === 'account_id' || at.part === 'stack' || at.part === 'min_confidence') more = true;
    } finally {
      saving = false;
    }
  }
</script>

{#snippet why(part: string)}
  {#if problem?.part === part}
    <p id="b-problem" role="alert" class="w-full text-[12.5px] text-trash">{problem.message}</p>
  {/if}
{/snippet}

<div class="flex flex-wrap items-start gap-[18px]">
  <section aria-label="Condition builder" class="card flex min-w-0 flex-[3_1_520px] flex-col gap-[18px] p-5">
    <div class="flex flex-col gap-1.5">
      <label for="b-name" class="text-[13px] font-semibold">Rule name</label>
      <input id="b-name" class="field h-11 max-w-[420px]" bind:value={b.name} placeholder="For example: Invoices" {...bad('name')} />
      {@render why('name')}
    </div>

    <div class="flex flex-col gap-2.5">
      {#if b.rows.length}
        <div class="flex flex-wrap items-center gap-2 font-semibold">
          <span>When an email matches</span>
          <select aria-label="All or any" class="field h-9 px-2.5 font-semibold" bind:value={b.match}>
            <option value="all">all</option>
            <option value="any">any</option>
          </select>
          <span>of these conditions</span>
        </div>
      {:else}
        <h3 class="font-semibold">Conditions (optional)</h3>
      {/if}
      {#each b.rows as row, i}
        {@const def = fields[row.field]}
        <div class="flex flex-wrap items-center gap-2 rounded-md border {problem?.part === 'row' + i ? 'border-trash' : 'border-line-divider'} bg-selected-row p-2.5">
          <span class="w-11 font-mono text-[12.5px] text-muted">{i === 0 ? 'where' : b.match === 'all' ? 'and' : 'or'}</span>
          <select aria-label="Field" class="field flex-[1_1_180px] px-2.5" {...bad('row' + i, 'field')} value={row.field} onchange={(e) => setField(i, e.currentTarget.value)}>
            {#each Object.entries(fields) as [id, f] (id)}
              <option value={id}>{f.label}</option>
            {/each}
          </select>
          <select aria-label="Operator" class="field flex-[1_1_150px] px-2.5" {...bad('row' + i, 'op')} bind:value={row.op}>
            {#each opsFor(def.type) as [id, name] (id)}
              <option value={id}>{name}</option>
            {/each}
          </select>
          {#if def.type !== 'bool'}
            <input aria-label="Value" class="field min-w-0 flex-[2_1_200px] font-mono text-[13px]" {...bad('row' + i, 'value')} bind:value={row.value} placeholder={def.ph} />
          {/if}
          <button type="button" aria-label="Remove condition {i + 1}" class="grid size-10 place-items-center rounded border border-line-card bg-surface p-0 text-muted" onclick={() => b.rows.splice(i, 1)}>
            <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12" /></svg>
          </button>
          {@render why('row' + i)}
        </div>
      {/each}
      <button type="button" class="inline-flex min-h-10 items-center gap-1.5 self-start rounded border border-dashed border-line-input bg-selected-row px-3.5 font-semibold" onclick={() => b.rows.push(b.rows.length ? { field: 'subject', op: 'contains_any', value: '' } : { field: 'from_domain', op: 'in', value: '' })}>
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
        Add condition
      </button>
    </div>

    <div class="flex flex-col gap-1.5">
      <label for="b-intent" class="text-[13px] font-semibold">And the email is about <span class="font-normal text-muted">(checked by AI; if you add conditions, only after they match)</span></label>
      <input id="b-intent" class="field h-11" bind:value={b.intent} placeholder="For example: an invoice or payment request" {...bad('intent')} />
      {@render why('intent')}
    </div>
    <label class="flex items-center gap-2.5"><input type="checkbox" bind:checked={b.unless} {...bad('unless')} />Except when I've replied to the sender before</label>
    {@render why('unless')}

    <div class="flex flex-col gap-2 border-t border-line-divider pt-3.5">
      <div class="font-semibold">Then</div>
      <div class="flex flex-wrap items-center gap-2">
        <select aria-label="Action" class="field h-11 flex-[1_1_180px] px-2.5" {...bad('action')} bind:value={b.action}>
          <option value="move">Move to folder</option>
          <option value="archive">Archive</option>
          <option value="trash">Move to Trash</option>
          <option value="keep">Keep in Inbox</option>
          <option value="flag">Keep in Inbox and flag</option>
        </select>
        {#if b.action === 'move'}
          <input aria-label="Folder" list="b-folders" class="field h-11 flex-[1_1_180px]" {...bad('folder')} bind:value={b.folder} placeholder="Folder name" />
          <datalist id="b-folders">
            {#each folders as f (f)}
              <option value={f}></option>
            {/each}
          </datalist>
        {/if}
      </div>
      {@render why('action')}
      {@render why('folder')}
      <label class="flex items-center gap-2.5"><input type="checkbox" bind:checked={b.markRead} />Also mark as read</label>
    </div>

    <details class="border-t border-line-divider pt-3.5" bind:open={more}>
      <summary class="min-h-8 cursor-pointer font-semibold">More options: mailbox, stacking{b.intent.trim() ? ', threshold' : ''}</summary>
      <div class="flex flex-col gap-3.5 pt-3">
        <MoreOptions id="b" value={b} onchange={(p) => Object.assign(b, p)} stackLabel="Also apply when another rule already matched (stacks)" {problem} />
        {#if b.intent.trim()}
          <div class="flex max-w-[420px] flex-col gap-1.5">
            <Threshold id="b-thr" value={b.min_confidence} onchange={(v) => (b.min_confidence = v)} invalid={bad('min_confidence')} />
            {@render why('min_confidence')}
          </div>
        {/if}
      </div>
    </details>
    <div class="flex flex-wrap gap-2 border-t border-line-divider pt-3.5">
      <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={saving} onclick={save}>{saving ? 'Saving…' : b.editingId ? 'Update rule' : 'Save rule'}</button>
      <TestRunner run={test} tall buttonClass="btn min-h-11 px-4 font-semibold" />
      {#if b.editingId}
        <a href="#/rules?id={b.editingId}" class="inline-flex min-h-11 items-center px-3.5 text-secondary">Cancel</a>
      {:else}
        <button type="button" class="min-h-11 border-0 bg-transparent px-3.5 text-secondary" onclick={() => (b = emptyBuilder())}>Clear</button>
      {/if}
    </div>
  </section>

  <aside aria-label="Rule preview" class="card flex min-w-0 flex-[2_1_300px] flex-col gap-3.5 p-[18px]">
    <div class="text-xs font-medium tracking-[0.06em] text-muted uppercase">In plain words</div>
    <p class="text-[15px] leading-[1.55]">{english(b, accounts.list.find((a) => String(a.id) === String(b.account_id))?.label)}</p>
    <div class="flex flex-wrap gap-1.5">
      {#each filled(b) as r}
        <span class="rounded bg-neutral px-2 py-1 font-mono text-[12.5px] text-ink-soft">{condText(toCondition(r))}</span>
      {/each}
    </div>
    {#if b.intent.trim()}
      <div class="rounded bg-both-bg px-3 py-2.5 text-[13px] text-both">Conditions run first for free; only matching emails go to the decision model (about $0.00002 each).</div>
    {:else}
      <div class="rounded bg-selected px-3 py-2.5 text-[13px]">Conditions only: decided instantly on your server, no model, no cost.</div>
    {/if}
    {#if tested.form === form && tested.ok}
      <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]">{tested.text}</div>
    {:else if tested.form === form && tested.text}
      <div role="alert" class="rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">{tested.text}</div>
    {:else if tested.form === form}
      <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]"><a href="#/accounts" class="font-semibold underline">Connect a mailbox</a> to test rules.</div>
    {/if}
    <div class="text-[12.5px] text-muted">Rules are checked top to bottom. New rules go to the bottom; reorder them in Rules.</div>
  </aside>
</div>
