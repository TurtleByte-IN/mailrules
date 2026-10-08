<script lang="ts">
  import { accounts, statuses } from '../state/accounts.svelte';

  /**
   * Which mailboxes a manual run covers: one checkbox each, and "All mailboxes". A mailbox that is not
   * connected cannot be run over, so it is shown with its status and cannot be ticked. The screen owns
   * the choice and applies `onchange`. `disabled` locks every box.
   */
  let { picked, disabled = false, onchange }: { picked: string[]; disabled?: boolean; onchange: (ids: string[]) => void } = $props();

  const usable = $derived(accounts.list.filter((a) => a.status === 'live'));
  const every = $derived(usable.length > 0 && usable.every((a) => picked.includes(String(a.id))));

  function toggle(id: string, on: boolean) {
    onchange(on ? [...picked, id] : picked.filter((x) => x !== id));
  }
</script>

<fieldset class="flex min-w-0 flex-col gap-1 border-0 p-0" {disabled}>
  <legend class="mb-1 p-0 text-[13px] font-semibold">Mailboxes</legend>
  <div class="flex flex-wrap gap-x-5 gap-y-1">
    <label class="flex min-h-8 items-center gap-2 max-md:min-h-11">
      <input type="checkbox" checked={every} onchange={(e) => onchange(e.currentTarget.checked ? usable.map((a) => String(a.id)) : [])} />
      All mailboxes
    </label>
    {#each accounts.list as a (a.id)}
      {@const live = a.status === 'live'}
      <label class="flex min-h-8 items-center gap-2 max-md:min-h-11 {live ? '' : 'text-muted'}">
        <input type="checkbox" checked={picked.includes(String(a.id))} disabled={!live} onchange={(e) => toggle(String(a.id), e.currentTarget.checked)} />
        <span class="break-all">{a.label}</span>
        {#if !live}<span class="text-[12.5px]">· {statuses[a.status].label}</span>{/if}
      </label>
    {/each}
  </div>
</fieldset>
