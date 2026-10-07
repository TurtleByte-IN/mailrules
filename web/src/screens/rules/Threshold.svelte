<script lang="ts">
  import { confidence } from '../../lib/format';

  // How sure the decision model must be before a rule acts. Shared by the rule editor and the
  // condition builder so both say it the same way; `id` keeps their control ids apart.
  // `value` null is the default from Settings. `invalid` marks the slider when a save named it.
  let { id, value, disabled = false, onchange, invalid = {} }: { id: string; value: number | null; disabled?: boolean; onchange: (v: number) => void; invalid?: Record<string, unknown> } = $props();

  // The slider's value while it is being dragged; handed over on release.
  let dragging = $state<number | null>(null);
  const sure = $derived(dragging ?? value);
</script>

<label for={id} class="text-[13px] font-semibold">Act when sure above {sure === null ? 'your default' : confidence(sure)}</label>
<input
  {id}
  type="range"
  min="50"
  max="99"
  class="h-10 accent-ink"
  {disabled}
  {...invalid}
  value={Math.round((value ?? 0.75) * 100)}
  oninput={(e) => (dragging = +e.currentTarget.value / 100)}
  onchange={(e) => {
    dragging = null;
    onchange(+e.currentTarget.value / 100);
  }}
/>
