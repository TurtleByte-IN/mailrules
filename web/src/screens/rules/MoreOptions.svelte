<script lang="ts">
  import type { Extras } from '../../lib/api/rules';
  import { features } from '../../lib/features';
  import { accounts } from '../../lib/state/accounts.svelte';

  // Shared by the rule editor and the condition builder; `id` keeps their control ids apart.
  let {
    id,
    value,
    onchange,
    stackLabel,
    draftLabel,
  }: { id: string; value: Extras; onchange: (patch: Partial<Extras>) => void; stackLabel: string; draftLabel: string } = $props();

  const later = $derived(value.later ?? { on: false, after: '7 days', action: 'archive' });
</script>

<div class="flex flex-wrap items-center gap-2">
  <label for="{id}-acct" class="w-[110px] text-[13px] font-semibold">Applies to</label>
  <select id="{id}-acct" class="field flex-[1_1_180px] px-2.5" value={value.account_id ?? ''} onchange={(e) => onchange({ account_id: e.currentTarget.value || null })}>
    <option value="">All mailboxes</option>
    {#each accounts.list as a (a.id)}
      <option value={a.id}>{a.email}</option>
    {/each}
  </select>
</div>
<label class="flex items-center gap-2.5 text-[13.5px]">
  <input type="checkbox" checked={value.stack} onchange={() => onchange({ stack: !value.stack })} />{stackLabel}
</label>
{#if features.timedActions}
  <div class="flex flex-wrap items-center gap-2">
    <label class="flex items-center gap-2.5 text-[13.5px]">
      <input type="checkbox" checked={later.on} onchange={() => onchange({ later: { ...later, on: !later.on } })} />Later, after
    </label>
    <select aria-label="Delay" class="field h-9 px-2" disabled={!later.on} value={later.after} onchange={(e) => onchange({ later: { ...later, after: e.currentTarget.value } })}>
      <option>24 hours</option><option>3 days</option><option>7 days</option><option>30 days</option>
    </select>
    <select aria-label="Later action" class="field h-9 px-2" disabled={!later.on} value={later.action} onchange={(e) => onchange({ later: { ...later, action: e.currentTarget.value } })}>
      <option value="archive">archive it</option><option value="trash">move it to Trash</option><option value="read">mark it read</option>
    </select>
  </div>
{/if}
{#if features.notifications}
  <div class="flex flex-wrap items-center gap-2">
    <label for="{id}-notify" class="w-[110px] text-[13px] font-semibold">Notify me</label>
    <select id="{id}-notify" class="field flex-[1_1_180px] px-2.5" value={value.notify ?? 'none'} onchange={(e) => onchange({ notify: e.currentTarget.value })}>
      <option value="none">No notification</option><option value="Telegram">On Telegram</option><option value="Slack">On Slack</option><option value="webhook">Call my webhook</option>
    </select>
  </div>
{/if}
{#if features.draftReplies}
  <label class="flex items-center gap-2.5 text-[13.5px]">
    <input type="checkbox" checked={!!value.draft} onchange={() => onchange({ draft: !value.draft })} />{draftLabel}
  </label>
  {#if value.draft}
    <input aria-label="How should the reply sound?" class="field" value={value.draftNote ?? ''} onchange={(e) => onchange({ draftNote: e.currentTarget.value })} placeholder="For example: polite no, not looking right now" />
  {/if}
{/if}
