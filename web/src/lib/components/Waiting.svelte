<script lang="ts">
  import { onMount } from 'svelte';

  /** What the daemon is doing, as a sentence without its closing dots. */
  let { text, after = 4 }: { text: string; after?: number } = $props();

  // Seconds since this appeared. Shown once it has taken a few; kept out of the live region so a
  // screen reader says the sentence once, not every second.
  let seconds = $state(0);
  onMount(() => {
    const timer = setInterval(() => seconds++, 1000);
    return () => clearInterval(timer);
  });
</script>

<div class="rounded bg-selected px-3 py-2.5 text-[13px]">
  <span role="status">{text}…</span>
  {#if seconds >= after}<span class="font-mono text-secondary" aria-hidden="true">{seconds}s</span>{/if}
</div>
