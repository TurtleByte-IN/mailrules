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

  // The WAI-ARIA tabs pattern: only the shown tab is in the Tab order; the arrow keys, Home and End
  // move to another tab and show it at once.
  let tabs: HTMLButtonElement[] = [];
  function onkeydown(e: KeyboardEvent) {
    const at = modes.findIndex(([id]) => id === mode);
    const to = { ArrowRight: at + 1, ArrowLeft: at - 1 + modes.length, Home: 0, End: modes.length - 1 }[e.key];
    if (to === undefined) return;
    e.preventDefault();
    mode = modes[to % modes.length][0];
    tabs[to % modes.length].focus();
  }
</script>

<div class="flex max-w-[980px] flex-col gap-[18px]">
  <header>
    <h1>Add rules</h1>
    <p class="mt-1 text-secondary">Describe rules in your own words, build one from exact conditions, or let the AI suggest rules from the mail you already have.</p>
  </header>
  <!-- Two by two below lg (and on a phone): the four labels only fit in one row from lg up. -->
  <div role="tablist" aria-label="How to add rules" class="inline-flex gap-1 self-start rounded-md bg-line-divider p-1 max-lg:grid max-lg:grid-cols-2 max-lg:self-stretch">
    {#each modes as [id, label], i (id)}
      <!-- Only the shown panel exists, so only its tab names it in aria-controls. -->
      <button
        bind:this={tabs[i]}
        type="button"
        role="tab"
        id="compose-tab-{id}"
        aria-selected={mode === id}
        aria-controls={mode === id ? 'compose-panel' : undefined}
        tabindex={mode === id ? 0 : -1}
        class="min-h-10 rounded border px-4 font-semibold whitespace-nowrap max-md:min-h-11 max-md:px-2 max-md:whitespace-normal {mode === id ? 'border-ink bg-surface text-ink' : 'border-transparent bg-transparent text-secondary'}"
        onclick={() => (mode = id)}
        {onkeydown}>{label}</button>
    {/each}
  </div>
  <div role="tabpanel" id="compose-panel" aria-labelledby="compose-tab-{mode}" class="flex flex-col gap-[18px]">
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
</div>
