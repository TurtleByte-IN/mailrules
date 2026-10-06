<script lang="ts">
  import { leaves, type Action, type Condition } from '../../lib/api/rules';
  import { actionsText, condText, treeWords } from '../rules/text';

  /** A rule as plain lines: when, match, unless, then. Used by draft cards and the rewrite comparison. */
  let { rule }: { rule: { intent?: string | null; conditions: Condition; exceptions: Condition; actions: Action[]; new_folders?: string[] } } = $props();
</script>

<div class="flex min-w-0 flex-col gap-1.5 text-[13.5px]">
  {#if rule.intent}
    <div><span class="text-muted">When</span> {rule.intent}</div>
  {/if}
  {#if leaves(rule.conditions).length}
    <div class="flex flex-wrap items-center gap-1.5">
      <span class="text-muted">Match</span>
      {#each leaves(rule.conditions) as c}
        <span class="rounded bg-neutral px-2 py-[3px] font-mono text-[12.5px] text-ink-soft">{condText(c)}</span>
      {/each}
    </div>
  {/if}
  {#if treeWords(rule.exceptions)}
    <div><span class="text-muted">Unless</span> {treeWords(rule.exceptions)}</div>
  {/if}
  <div>
    <span class="text-muted">Then</span>
    {actionsText(rule.actions)}
    {#each rule.new_folders ?? [] as f}
      <span class="chip chip-both ml-2 font-normal">new folder: {f}</span>
    {/each}
  </div>
</div>
