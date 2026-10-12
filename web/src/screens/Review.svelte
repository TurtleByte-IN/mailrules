<script lang="ts">
  import { link } from 'svelte-spa-router';
  import { limits, load as loadLimits } from '../lib/state/limits.svelte';
  import { load, loadMore, review } from '../lib/state/review.svelte';
  import LoadError from './activity/LoadError.svelte';
  import Card from './review/Card.svelte';

  load();
  loadLimits();
</script>

<div class="flex max-w-[920px] flex-col gap-[18px]">
  <header>
    <h1>Needs review</h1>
    <p class="mt-1 text-secondary">MailRules wasn't sure about these, so they're still in your inbox. Each answer teaches it.</p>
  </header>
  {#if limits.value?.ai_emails?.paused}
    {@const until = new Date(limits.value.ai_emails.resets_at * 1000).toLocaleDateString([], { day: 'numeric', month: 'long', timeZone: 'UTC' })}
    <div role="status" class="rounded border border-warn-line bg-warn-bg px-4 py-3 text-[13.5px] text-warn">
      This month's AI-sorted emails are used up, so until {until} the emails that need AI wait here for you. Sender rules and rules built from conditions keep sorting.
      <a href="#/settings" class="font-semibold underline">Add your own model key</a>{#if limits.value.upgrade_url}{' or '}<a href={limits.value.upgrade_url} class="font-semibold underline">upgrade</a>{/if} to have AI sort new mail again.
    </div>
  {/if}
  {#if review.error}
    <LoadError message={review.error} retry={load} />
  {:else if review.loaded && !review.list.length}
    <div class="card px-6 py-10 text-center">
      <div class="text-lg font-semibold">All clear</div>
      <p class="mt-1.5 mb-4 text-secondary">Nothing waiting. New uncertain emails will show up here.</p>
      <a href="/activity" use:link class="btn no-underline">Back to activity</a>
    </div>
  {/if}
  {#each review.list as item (item.id)}
    <Card {item} />
  {/each}
  {#if review.next}
    <button type="button" class="btn self-center" onclick={loadMore}>Load more</button>
  {/if}
</div>
