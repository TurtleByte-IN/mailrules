<script lang="ts">
  import { addTemplate, compose, loadTemplates } from '../../lib/state/compose.svelte';
  import { rules } from '../../lib/state/rules.svelte';
  import { actionsText, kind } from '../rules/text';

  loadTemplates();
</script>

<div class="grid grid-cols-[repeat(auto-fill,minmax(260px,1fr))] gap-3">
  {#each compose.templates as g (g.id)}
    {@const k = kind(g)}
    {@const added = rules.list.some((r) => r.name === g.name)}
    <article class="card flex flex-col gap-2 p-4">
      <div class="flex items-center justify-between gap-2"><span class="text-[15px] font-semibold">{g.name}</span><span class={k.chip}>{k.label}</span></div>
      <p class="flex-1 text-[13px] text-secondary">{g.desc}</p>
      <div class="text-[12.5px] text-nav"><span class="text-muted">Then</span> {actionsText(g.actions)}</div>
      <button type="button" class="min-h-[38px] self-start rounded border px-3.5 font-semibold {added ? 'border-line-card bg-neutral text-muted' : 'border-ink bg-ink text-surface'}" disabled={added} onclick={() => addTemplate(g)}>{added ? 'Added' : 'Add rule'}</button>
    </article>
  {/each}
</div>
