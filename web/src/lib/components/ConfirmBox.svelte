<script lang="ts">
  import { onMount } from 'svelte';

  /**
   * An inline confirmation in the flow of the page (never an overlay, so it fits a phone). Focus moves
   * to Cancel when it opens and goes back to the control that opened it when it closes; Escape cancels.
   */
  let {
    question,
    note = '',
    confirm,
    onconfirm,
    oncancel,
  }: { question: string; note?: string; confirm: string; onconfirm: () => void; oncancel: () => void } = $props();

  const uid = $props.id();
  let box: HTMLElement;
  let cancel: HTMLButtonElement;

  onMount(() => {
    const opener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
    cancel.focus();
    return () => {
      // Only when focus is still ours to give: inside the box, or lost to the page as the box goes.
      const now = document.activeElement;
      if (opener?.isConnected && (!now || now === document.body || box.contains(now))) opener.focus();
    };
  });

  function onkeydown(e: KeyboardEvent) {
    if (e.key !== 'Escape') return;
    e.stopPropagation();
    oncancel();
  }
</script>

<div
  bind:this={box}
  role="alertdialog"
  aria-labelledby="{uid}-q"
  aria-describedby={note ? `${uid}-n` : undefined}
  tabindex="-1"
  class="flex w-full flex-wrap items-center justify-between gap-x-4 gap-y-2 rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn"
  {onkeydown}
>
  <div class="flex min-w-0 flex-[1_1_240px] flex-col gap-0.5">
    <span id="{uid}-q" class="font-semibold">{question}</span>
    {#if note}<span id="{uid}-n">{note}</span>{/if}
  </div>
  <span class="flex flex-wrap gap-2">
    <button bind:this={cancel} type="button" class="btn" onclick={oncancel}>Cancel</button>
    <button type="button" class="btn-primary" onclick={onconfirm}>{confirm}</button>
  </span>
</div>
