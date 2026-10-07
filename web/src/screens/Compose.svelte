<script lang="ts">
  import { router } from 'svelte-spa-router';
  import { compose } from '../lib/state/compose.svelte';
  import { rules } from '../lib/state/rules.svelte';
  import Build from './compose/Build.svelte';
  import Describe from './compose/Describe.svelte';
  import Suggest from './compose/Suggest.svelte';
  import Templates from './compose/Templates.svelte';

  // Rules links here with ?mode=build for a new condition rule and ?edit=<id> to edit one.
  const query = $derived(new URLSearchParams(router.querystring));
  // Needs review links here with ?idea=<text> to start a rule from an email.
  const idea = new URLSearchParams(router.querystring).get('idea');
  if (idea) compose.text = idea.slice(0, 4000);

  const editing = $derived(rules.list.find((r) => String(r.id) === query.get('edit')));

  const modes = [
    ['describe', 'Describe it'],
    ['build', 'Build with conditions'],
    ['templates', 'Templates'],
    ['suggest', 'Suggest from my mail'],
  ] as const;
  let mode = $state<(typeof modes)[number][0]>(/(^|&)(edit=|mode=build)/.test(router.querystring ?? '') ? 'build' : 'describe');
</script>

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Add rules</h1>
    <p class="mt-1 text-secondary">Describe rules in your own words, build one from exact conditions, or let the AI suggest rules from the mail you already have.</p>
  </header>
  <div role="group" aria-label="How to add rules" class="inline-flex gap-1 self-start rounded-md bg-line-divider p-1 max-md:grid max-md:grid-cols-2 max-md:self-stretch">
    {#each modes as [id, label] (id)}
      <button type="button" aria-pressed={mode === id} class="min-h-10 rounded border px-4 font-semibold max-md:min-h-11 max-md:px-2 {mode === id ? 'border-ink bg-surface text-ink' : 'border-transparent bg-transparent text-secondary'}" onclick={() => (mode = id)}>{label}</button>
    {/each}
  </div>
  {#if mode === 'templates'}
    <Templates />
  {:else if mode === 'suggest'}
    <Suggest />
  {:else if mode === 'describe'}
    <Describe />
  {:else}
    {#key editing?.id}
      <Build rule={editing} />
    {/key}
  {/if}
</div>
