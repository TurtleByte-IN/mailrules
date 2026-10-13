<script lang="ts">
  import { accounts } from '../../lib/state/accounts.svelte';
  import { load as loadNotifications, notifications } from '../../lib/state/notifications.svelte';
  import { settings } from '../../lib/state/settings.svelte';
  import type { Extras } from './text';

  // Shared by the rule editor and the condition builder; `id` keeps their control ids apart.
  // `problem` is a refused save to show on one of these controls ("account_id", "stack", "notify" or "draft").
  let { id, value, onchange, stackLabel, draftLabel = 'Draft a reply (never sent automatically)', problem }: { id: string; value: Extras; onchange: (patch: Partial<Extras>) => void; stackLabel: string; draftLabel?: string; problem?: { part: string; message: string } | null } = $props();
  const bad = (part: string) => (problem?.part === part ? { 'aria-invalid': true, 'aria-describedby': id + '-problem' } : {});

  // Notifications exist only in a build with their module; the destinations are fetched once
  // here, and kept current by notifications.changed after that.
  const notifying = $derived(settings.value.features.notifications);
  $effect(() => {
    if (notifying && !notifications.loaded) loadNotifications();
  });
  // Draft replies exist only in a build (and plan) with their module.
  const drafting = $derived(settings.value.features.draft_replies);

  /** The rule's destinations with `dest` ticked or unticked, in the order Settings lists them. */
  function toggle(dest: number) {
    const on = (d: number) => (d === dest ? !value.notify.includes(d) : value.notify.includes(d));
    onchange({ notify: notifications.destinations.map((d) => d.id).filter(on) });
  }
</script>

<div class="flex flex-wrap items-center gap-2">
  <label for="{id}-acct" class="w-[110px] text-[13px] font-semibold">Applies to</label>
  <!-- A rule whose mailbox was removed has none chosen yet, so picking All mailboxes is a change too. -->
  <select id="{id}-acct" class="field flex-[1_1_180px] px-2.5" {...bad('account_id')} value={value.mailbox_removed ? 'removed' : String(value.account_id ?? '')} onchange={(e) => onchange({ account_id: e.currentTarget.value ? Number(e.currentTarget.value) : null })}>
    {#if value.mailbox_removed}
      <option value="removed" disabled>Choose a mailbox</option>
    {/if}
    <option value="">All mailboxes</option>
    {#each accounts.list as a (a.id)}
      <option value={String(a.id)}>{a.label}</option>
    {/each}
  </select>
</div>
<label class="flex items-center gap-2.5 text-[13.5px]">
  <input type="checkbox" checked={value.stack} {...bad('stack')} onchange={() => onchange({ stack: !value.stack })} />{stackLabel}
</label>
{#if notifying}
  <div role="group" aria-labelledby="{id}-notify" class="flex flex-wrap items-start gap-2">
    <span id="{id}-notify" class="w-[110px] text-[13px] font-semibold">Notify</span>
    <div class="flex flex-[1_1_180px] flex-col gap-1.5">
      {#each notifications.destinations as d (d.id)}
        <label class="flex items-center gap-2.5 text-[13.5px]">
          <input type="checkbox" checked={value.notify.includes(d.id)} {...bad('notify')} onchange={() => toggle(d.id)} />{d.name}
        </label>
      {:else}
        {#if notifications.error}
          <p class="text-[12.5px] text-trash">Could not load where notifications go. {notifications.error}</p>
        {:else if notifications.loaded}
          <p class="text-[12.5px] text-secondary">No destinations yet. Add one in <a href="#/settings" class="underline">Settings</a>, under Notifications and webhooks.</p>
        {/if}
      {/each}
    </div>
  </div>
{/if}
{#if drafting}
  <label class="flex items-center gap-2.5 text-[13.5px]">
    <input type="checkbox" checked={!!value.draft} {...bad('draft')} onchange={() => onchange({ draft: value.draft ? null : { instruction: '', read_previous: false } })} />{draftLabel}
  </label>
  {#if value.draft}
    {@const d = value.draft}
    <input class="field" aria-label="How should the reply sound?" placeholder="For example: polite no, not looking right now" maxlength="300" value={d.instruction} {...bad('draft')} onchange={(e) => onchange({ draft: { ...d, instruction: e.currentTarget.value } })} />
    <label class="flex items-center gap-2.5 text-[13.5px]">
      <input type="checkbox" checked={d.read_previous} {...bad('draft')} onchange={() => onchange({ draft: { ...d, read_previous: !d.read_previous } })} />Also read the previous message in the thread
    </label>
  {/if}
{/if}
{#if problem?.part === 'account_id' || problem?.part === 'stack' || problem?.part === 'notify' || problem?.part === 'draft'}
  <p id="{id}-problem" role="alert" class="text-[12.5px] text-trash">{problem.message}</p>
{/if}
