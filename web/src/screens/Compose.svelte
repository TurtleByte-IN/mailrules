<script lang="ts">
  import { router } from 'svelte-spa-router';
  import { rules } from '../lib/state/rules.svelte';
  import Build from './compose/Build.svelte';
  import Describe from './compose/Describe.svelte';
  import Templates from './compose/Templates.svelte';

  // Rules links here with ?mode=build for a new condition rule and ?edit=<id> to edit one.
  const query = $derived(new URLSearchParams(router.querystring));
  const editing = $derived(rules.list.find((r) => r.id === query.get('edit')));

  const modes = [
    ['describe', 'Describe it'],
    ['build', 'Build with conditions'],
    ['templates', 'Templates'],
  ] as const;
  let mode = $state<(typeof modes)[number][0]>(/(^|&)(edit=|mode=build)/.test(router.querystring ?? '') ? 'build' : 'describe');
</script>

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Add rules</h1>
    <p class="mt-1 text-secondary">Describe rules in your own words, or build one from exact conditions.</p>
  </header>
  <div role="group" aria-label="How to add rules" class="inline-flex gap-1 self-start rounded-md bg-line-divider p-1">
    {#each modes as [id, label] (id)}
      <button type="button" aria-pressed={mode === id} class="min-h-10 rounded border px-4 font-semibold {mode === id ? 'border-ink bg-surface text-ink' : 'border-transparent bg-transparent text-secondary'}" onclick={() => (mode = id)}>{label}</button>
    {/each}
  </div>
  {#if mode === 'templates'}
    <Templates />
  {:else if mode === 'describe'}
    <Describe />
  {:else}
    {#key editing?.id}
      <Build rule={editing} />
    {/key}
  {/if}
</div>
