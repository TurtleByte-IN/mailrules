<script lang="ts">
  import { modelChoice, modelNames } from './text';

  // The model that decides a rule's plain-English part. A listed model is saved as soon as it is
  // picked. OpenAI, Ollama and "Other" ask for a `name:model` first, and the daemon's own refusal
  // (its CheckModel text) is shown beside the field by the parent. A stored value the list does
  // not know stays selected as it is; nothing changes it until the user sets another.
  let { id, value, onchange, invalid = {} }: { id: string; value: string; onchange: (v: string) => Promise<void> | void; invalid?: Record<string, unknown> } = $props();

  // The choice made in the list that is not saved yet, and the name typed for it.
  let choosing = $state<string | null>(null);
  // svelte-ignore state_referenced_locally
  let typed = $state(modelChoice(value) in modelNames ? '' : value);
  const choice = $derived(choosing ?? modelChoice(value));
  const custom = $derived(!(choice in modelNames));
  const hints: Record<string, string> = { openai: 'openai:gpt-4o-mini', ollama: 'ollama:llama3.2', other: 'name:model' };

  function pick(v: string) {
    if (v in modelNames) {
      choosing = null;
      return set(v);
    }
    choosing = v;
    if (modelChoice(value) !== v) typed = v === 'other' ? '' : v + ':';
  }

  async function set(v: string) {
    await onchange(v);
    // Saved: the list follows the stored value again. Refused: the choice and the text stay for a fix.
    if (value === v) choosing = null;
  }
</script>

<label for={id} class="text-[13px] font-semibold">Model</label>
<select {id} class="field px-2.5" value={choice} onchange={(e) => pick(e.currentTarget.value)} {...invalid}>
  {#each Object.entries(modelNames) as [v, name] (v)}
    <option value={v}>{name}</option>
  {/each}
  <option value="openai">OpenAI-compatible…</option>
  <option value="ollama">Ollama…</option>
  <option value="other">Other (name:model)…</option>
</select>
{#if custom}
  <div class="flex flex-wrap gap-2">
    <input
      class="field min-w-0 flex-[1_1_150px] font-mono"
      aria-label="Model as name:model"
      placeholder={hints[choice]}
      autocomplete="off"
      spellcheck="false"
      bind:value={typed}
      onkeydown={(e) => e.key === 'Enter' && typed.trim() && typed.trim() !== value && set(typed.trim())}
      {...invalid}
    />
    <button type="button" class="btn font-semibold" disabled={!typed.trim() || typed.trim() === value} onclick={() => set(typed.trim())}>Set model</button>
  </div>
{/if}
