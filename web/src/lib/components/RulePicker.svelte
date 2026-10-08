<script lang="ts">
  import type { Rule } from '../api/rules';

  /**
   * Which rules a manual run checks: every rule, or only the ticked ones, still in their usual order.
   * `ids` is null for every rule. `rules` are the rules that are switched on, in the order they are checked.
   * The screen owns the choice and applies `onchange`. `disabled` locks every control.
   */
  let { ids, rules, disabled = false, onchange }: { ids: number[] | null; rules: Rule[]; disabled?: boolean; onchange: (ids: number[] | null) => void } = $props();

  const uid = $props.id();

  function toggle(id: number, on: boolean) {
    const next = on ? [...(ids ?? []), id] : (ids ?? []).filter((x) => x !== id);
    onchange(rules.filter((r) => next.includes(r.id)).map((r) => r.id)); // kept in the order they are checked
  }
</script>

<fieldset class="flex min-w-0 flex-col gap-1 border-0 p-0" {disabled}>
  <legend class="mb-1 p-0 text-[13px] font-semibold">Rules</legend>
  <div class="flex flex-wrap gap-x-5 gap-y-1">
    <label class="flex min-h-8 items-center gap-2 max-md:min-h-11"><input type="radio" name="{uid}-which" checked={ids === null} onchange={() => onchange(null)} />Every rule</label>
    <label class="flex min-h-8 items-center gap-2 max-md:min-h-11">
      <input type="radio" name="{uid}-which" checked={ids !== null} onchange={() => onchange(rules.map((r) => r.id))} />Only some rules
    </label>
  </div>
  {#if ids !== null}
    <ul class="flex flex-wrap gap-x-5 gap-y-1 pl-6 max-md:pl-0" aria-label="Rules to check">
      {#each rules as r (r.id)}
        <li>
          <label class="flex min-h-8 items-center gap-2 max-md:min-h-11">
            <input type="checkbox" checked={ids.includes(r.id)} onchange={(e) => toggle(r.id, e.currentTarget.checked)} />
            <span class="break-words">{r.name}</span>
          </label>
        </li>
      {/each}
    </ul>
  {/if}
  <p class="text-[12.5px] text-muted">
    Only the rules you pick are checked, in their usual order. Sender rules, your own “always keep” and “always trash” answers, apply either way.
  </p>
</fieldset>
