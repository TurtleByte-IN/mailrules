<script lang="ts">
  import { onDestroy, type Snippet } from 'svelte';
  import { listen, supported } from './dictation';

  /** The box people type or dictate into, with the Dictate button and its two fallback messages. `actions` sit beside Dictate. */
  let { id, label, placeholder, value = $bindable(), actions }: { id: string; label: string; placeholder: string; value: string; actions?: Snippet } = $props();

  const canDictate = supported();
  // Set while listening.
  let stop = $state<(() => void) | null>(null);
  let micBlocked = $state(false);

  function toggleMic() {
    if (stop) return stop();
    const before = value.trim();
    micBlocked = false;
    stop = listen(
      (heard) => (value = ((before ? before + ' ' : '') + heard).slice(0, 4000)),
      (error) => {
        stop = null;
        micBlocked = error === 'not-allowed' || error === 'service-not-allowed';
      },
    );
  }
  onDestroy(() => stop?.());
</script>

<label for={id} class="text-[13px] font-semibold">{label}</label>
<textarea {id} rows="5" maxlength="4000" bind:value {placeholder} class="resize-y rounded-md border border-line-input bg-surface p-3.5 text-[15px] leading-normal text-ink"></textarea>
<div class="flex flex-wrap items-center gap-2.5">
  <button type="button" aria-pressed={!!stop} disabled={!canDictate} class="btn min-h-11 px-4 font-semibold {stop ? 'border-trash bg-trash-bg text-trash' : ''}" onclick={toggleMic}>
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="3" width="6" height="11" rx="3" /><path d="M5 11a7 7 0 0 0 14 0M12 18v3" /></svg>
    {stop ? 'Listening… tap to stop' : 'Dictate'}
  </button>
  {@render actions?.()}
  <span class="ml-auto text-[12.5px] text-muted">{value.length} / 4000</span>
</div>
{#if !canDictate}
  <p role="status" class="text-[12.5px] text-muted">This browser has no speech recognition, so dictation is off. Type your rules instead.</p>
{:else if micBlocked}
  <p role="status" class="text-[12.5px] text-muted">The microphone is blocked for this page. Allow it in the browser, or type your rules instead.</p>
{/if}
