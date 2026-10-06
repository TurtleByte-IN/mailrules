<script lang="ts">
  import { push } from 'svelte-spa-router';
  import { notBuilt } from '../../lib/api/client';
  import * as rulesApi from '../../lib/api/rules';
  import NotBuilt from '../../lib/components/NotBuilt.svelte';
  import { accounts } from '../../lib/state/accounts.svelte';
  import { attempt } from '../../lib/state/compose.svelte';
  import { add, edit, rules } from '../../lib/state/rules.svelte';
  import { flash } from '../../lib/state/toast.svelte';
  import MoreOptions from '../rules/MoreOptions.svelte';
  import { condText, fields } from '../rules/text';
  import { emptyBuilder, english, filled, fromRule, opsFor, toCondition, toRule } from './builder';

  /** The saved rule being edited; none for a new rule. The parent re-creates this component when it changes. */
  let { rule }: { rule?: rulesApi.Rule } = $props();

  const start = () => (rule ? fromRule(rule) : emptyBuilder());
  let b = $state(start());
  let testing = $state(false);
  // A test result is shown only for the form it was run on.
  let tested = $state({ form: '', text: '', notBuilt: false });
  const form = $derived(JSON.stringify(toRule(b)));

  const empty = $derived(!filled(b).length && !b.intent.trim());
  // Folders other rules already move mail to. The mailbox's own folder list arrives with accounts.
  const folders = $derived([...new Set(rules.list.flatMap((r) => r.actions.flatMap((a) => a.folder ?? [])))]);

  function setField(i: number, field: string) {
    b.rows[i] = { field, op: opsFor(fields[field].type)[0][0], value: '' };
  }

  async function test() {
    if (empty) return flash('Add a condition with a value first');
    const account = b.account_id ?? accounts.list[0]?.id;
    if (account === undefined) return flash('Connect a mailbox first: a test runs on its recent mail');
    const r = toRule(b);
    testing = true;
    try {
      const t = await rulesApi.test({ account_id: Number(account), rules: [{ ...r, enabled: true }], limit: 200 });
      // A draft's rows carry its name and no rule id.
      const latest = t.results.filter((x) => x.rule_id === null && x.rule_name === r.name).sort((x, y) => (y.received_at ?? 0) - (x.received_at ?? 0))[0];
      tested = {
        form,
        notBuilt: false,
        text:
          `Matched ${t.matched} of your last ${t.tested} emails, ` +
          (t.model_calls ? `with ${t.model_calls} sent to the decision model.` : 'all decided by conditions with no model calls.') +
          (latest ? ` Latest: mail from ${latest.from}.` : ''),
      };
    } catch (e) {
      if (notBuilt(e)) tested = { form, text: '', notBuilt: true };
      else flash((e as Error).message);
    } finally {
      testing = false;
    }
  }

  async function save() {
    if (empty) return flash('Add at least one condition with a value');
    if (b.action === 'move' && !b.folder.trim()) return flash('Choose a folder to move these emails to');
    const r = toRule(b);
    const id = b.editingId;
    const saved = await attempt('Saving new rules', async () => (id ? edit(id, r) : (await add([{ ...r, said: 'Built with conditions', enabled: true }]))[0]));
    if (!saved) return;
    flash(r.name + (id ? ' updated' : ' saved and live'));
    push('/rules?id=' + saved.id);
  }
</script>

<div class="flex flex-wrap items-start gap-[18px]">
  <section aria-label="Condition builder" class="card flex min-w-0 flex-[3_1_520px] flex-col gap-[18px] p-5">
    <div class="flex flex-col gap-1.5">
      <label for="b-name" class="text-[13px] font-semibold">Rule name</label>
      <input id="b-name" class="field h-11 max-w-[420px]" bind:value={b.name} placeholder="For example: Invoices" />
    </div>

    <div class="flex flex-col gap-2.5">
      <div class="flex flex-wrap items-center gap-2 font-semibold">
        <span>When an email matches</span>
        <select aria-label="All or any" class="field h-9 px-2.5 font-semibold" bind:value={b.match}>
          <option value="all">all</option>
          <option value="any">any</option>
        </select>
        <span>of these conditions</span>
      </div>
      {#each b.rows as row, i}
        {@const def = fields[row.field]}
        <div class="flex flex-wrap items-center gap-2 rounded-md border border-line-divider bg-selected-row p-2.5">
          <span class="w-11 font-mono text-[12.5px] text-muted">{i === 0 ? 'where' : b.match === 'all' ? 'and' : 'or'}</span>
          <select aria-label="Field" class="field flex-[1_1_180px] px-2.5" value={row.field} onchange={(e) => setField(i, e.currentTarget.value)}>
            {#each Object.entries(fields) as [id, f] (id)}
              <option value={id}>{f.label}</option>
            {/each}
          </select>
          <select aria-label="Operator" class="field flex-[1_1_150px] px-2.5" bind:value={row.op}>
            {#each opsFor(def.type) as [id, name] (id)}
              <option value={id}>{name}</option>
            {/each}
          </select>
          {#if def.type !== 'bool'}
            <input aria-label="Value" class="field min-w-0 flex-[2_1_200px] font-mono text-[13px]" bind:value={row.value} placeholder={def.ph} />
          {/if}
          {#if b.rows.length > 1}
            <button type="button" aria-label="Remove condition {i + 1}" class="grid size-10 place-items-center rounded border border-line-card bg-surface p-0 text-muted" onclick={() => b.rows.splice(i, 1)}>
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true"><path d="M18 6 6 18M6 6l12 12" /></svg>
            </button>
          {/if}
        </div>
      {/each}
      <button type="button" class="inline-flex min-h-10 items-center gap-1.5 self-start rounded border border-dashed border-line-input bg-selected-row px-3.5 font-semibold" onclick={() => b.rows.push({ field: 'subject', op: 'contains_any', value: '' })}>
        <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" aria-hidden="true"><path d="M12 5v14M5 12h14" /></svg>
        Add condition
      </button>
    </div>

    <div class="flex flex-col gap-1.5">
      <label for="b-intent" class="text-[13px] font-semibold">And the email is about <span class="font-normal text-muted">(optional, checked by AI only after the conditions match)</span></label>
      <input id="b-intent" class="field h-11" bind:value={b.intent} placeholder="For example: an invoice or payment request" />
    </div>
    <label class="flex items-center gap-2.5"><input type="checkbox" bind:checked={b.unless} />Except when I've replied to the sender before</label>

    <div class="flex flex-col gap-2 border-t border-line-divider pt-3.5">
      <div class="font-semibold">Then</div>
      <div class="flex flex-wrap items-center gap-2">
        <select aria-label="Action" class="field h-11 flex-[1_1_180px] px-2.5" bind:value={b.action}>
          <option value="move">Move to folder</option>
          <option value="archive">Archive</option>
          <option value="trash">Move to Trash</option>
          <option value="keep">Keep in Inbox</option>
          <option value="flag">Keep in Inbox and flag</option>
        </select>
        {#if b.action === 'move'}
          <input aria-label="Folder" list="b-folders" class="field h-11 flex-[1_1_180px]" bind:value={b.folder} placeholder="Folder name" />
          <datalist id="b-folders">
            {#each folders as f (f)}
              <option value={f}></option>
            {/each}
          </datalist>
        {/if}
      </div>
      <label class="flex items-center gap-2.5"><input type="checkbox" bind:checked={b.markRead} />Also mark as read</label>
    </div>

    <details class="border-t border-line-divider pt-3.5">
      <summary class="min-h-8 cursor-pointer font-semibold">More options: mailbox, stacking</summary>
      <div class="flex flex-col gap-3.5 pt-3">
        <MoreOptions id="b" value={b} onchange={(p) => Object.assign(b, p)} stackLabel="Also apply when another rule already matched (stacks)" />
      </div>
    </details>
    <div class="flex flex-wrap gap-2 border-t border-line-divider pt-3.5">
      <button type="button" class="btn-primary min-h-11 px-[18px]" onclick={save}>{b.editingId ? 'Update rule' : 'Save rule'}</button>
      <button type="button" class="btn min-h-11 px-4 font-semibold" disabled={testing} onclick={test}>{testing ? 'Testing on last 200 emails…' : 'Test on last 200 emails'}</button>
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
    {#if tested.form === form && tested.notBuilt}
      <NotBuilt what="Testing a rule" />
    {:else if tested.form === form}
      <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]">{tested.text}</div>
    {/if}
    <div class="text-[12.5px] text-muted">Rules are checked top to bottom. New rules go to the bottom; reorder them in Rules.</div>
  </aside>
</div>
