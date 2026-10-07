<script lang="ts">
  let { on, label, onchange }: { on: boolean; label: string; onchange: () => void } = $props();
</script>

<button
  type="button"
  aria-pressed={on}
  aria-label={label}
  onclick={onchange}
  class="relative h-6 w-11 shrink-0 rounded-full border-0 p-0 after:absolute after:-inset-2.5 after:content-[''] {on ? 'bg-ink' : 'bg-line-input'}"
>
  <span class="absolute top-0.5 size-5 rounded-full bg-surface transition-[left] {on ? 'left-[22px]' : 'left-0.5'}"></span>
</button>
