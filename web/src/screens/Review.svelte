<script lang="ts">
  import { link } from 'svelte-spa-router';
  import { load, review } from '../lib/state/review.svelte';
  import Card from './review/Card.svelte';

  let loaded = $state(false);
  load().then(() => (loaded = true));
</script>

<div class="flex max-w-[920px] flex-col gap-[18px]">
  <header>
    <h1>Needs review</h1>
    <p class="mt-1 text-secondary">MailRules wasn't sure about these, so they're still in your inbox. Each answer teaches it.</p>
  </header>
  {#if loaded && !review.list.length}
    <div class="card px-6 py-10 text-center">
      <div class="text-lg font-semibold">All clear</div>
      <p class="mt-1.5 mb-4 text-secondary">Nothing waiting. New uncertain emails will show up here.</p>
      <a href="/" use:link class="btn no-underline">Back to activity</a>
    </div>
  {/if}
  {#each review.list as item (item.id)}
    <Card {item} />
  {/each}
</div>
