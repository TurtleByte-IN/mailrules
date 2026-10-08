<script lang="ts">
  import { accounts } from '../../lib/state/accounts.svelte';
  import type { Extras } from './text';

  // Shared by the rule editor and the condition builder; `id` keeps their control ids apart.
  // `problem` is a refused save to show on one of these two controls ("account_id" or "stack").
  let { id, value, onchange, stackLabel, problem }: { id: string; value: Extras; onchange: (patch: Partial<Extras>) => void; stackLabel: string; problem?: { part: string; message: string } | null } = $props();
  const bad = (part: string) => (problem?.part === part ? { 'aria-invalid': true, 'aria-describedby': id + '-problem' } : {});
</script>

<div class="flex flex-wrap items-center gap-2">
  <label for="{id}-acct" class="w-[110px] text-[13px] font-semibold">Applies to</label>
  <!-- A rule whose mailbox was removed has none chosen yet, so picking All mailboxes is a change too. -->
  <select id="{id}-acct" class="field flex-[1_1_180px] px-2.5" {...bad('account_id')} value={value.mailbox_removed ? 'removed' : String(value.account_id ?? '')} onchange={(e) => onchange({ account_id: e.currentTarget.value ? Number(e.currentTarget.value) : null })}>
    {#if value.mailbox_removed}
      <option value="removed" disabled>Choose a mailbox</option>
    {/if}
    <option value="">All mailboxes</option>
    {#each accounts.list as a (a.id)}
      <option value={String(a.id)}>{a.label}</option>
    {/each}
  </select>
</div>
<label class="flex items-center gap-2.5 text-[13.5px]">
  <input type="checkbox" checked={value.stack} {...bad('stack')} onchange={() => onchange({ stack: !value.stack })} />{stackLabel}
</label>
{#if problem?.part === 'account_id' || problem?.part === 'stack'}
  <p id="{id}-problem" role="alert" class="text-[12.5px] text-trash">{problem.message}</p>
{/if}
