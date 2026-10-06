<script lang="ts">
  import { accounts } from '../../lib/state/accounts.svelte';
  import type { Extras } from './text';

  // Shared by the rule editor and the condition builder; `id` keeps their control ids apart.
  let { id, value, onchange, stackLabel }: { id: string; value: Extras; onchange: (patch: Partial<Extras>) => void; stackLabel: string } = $props();
</script>

<div class="flex flex-wrap items-center gap-2">
  <label for="{id}-acct" class="w-[110px] text-[13px] font-semibold">Applies to</label>
  <select id="{id}-acct" class="field flex-[1_1_180px] px-2.5" value={String(value.account_id ?? '')} onchange={(e) => onchange({ account_id: e.currentTarget.value ? Number(e.currentTarget.value) : null })}>
    <option value="">All mailboxes</option>
    {#each accounts.list as a (a.id)}
      <option value={String(a.id)}>{a.label}</option>
    {/each}
  </select>
</div>
<label class="flex items-center gap-2.5 text-[13.5px]">
  <input type="checkbox" checked={value.stack} onchange={() => onchange({ stack: !value.stack })} />{stackLabel}
</label>
