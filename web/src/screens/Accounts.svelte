<script lang="ts">
  import type { Account } from '../lib/api/accounts';
  import { clock, day } from '../lib/format';
  import { accounts, load, loadPresets, reconnect, remove, setPaused, statuses } from '../lib/state/accounts.svelte';
  import Wizard from './accounts/Wizard.svelte';

  let connecting = $state(false);
  let removing = $state<number>();

  // The list names each provider by its preset label.
  if (!accounts.presets.length) loadPresets();

  const provider = (a: Account) => accounts.presets.find((p) => p.name === a.preset)?.label ?? a.host;
  const detail = (a: Account) =>
    a.status === 'live'
      ? a.capabilities.includes('IDLE') ? 'push (IDLE) connected' : 'checked once a minute'
      : a.last_error || statuses[a.status].label.toLowerCase();
</script>

<div class="flex max-w-[920px] flex-col gap-[18px]">
  <header class="flex flex-wrap items-end justify-between gap-4">
    <div>
      <h1>Mailboxes</h1>
      <p class="mt-1 text-secondary">Any IMAP mailbox. MailRules sorts on the server, so every app you use sees the result.</p>
    </div>
    {#if !connecting}
      <button type="button" class="btn-primary" onclick={() => (connecting = true)}>Add mailbox</button>
    {/if}
  </header>

  {#if accounts.error}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-[18px] py-3 text-trash">
      <span>Could not load your mailboxes. {accounts.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {/if}

  {#if accounts.loaded}
    <section aria-label="Connected mailboxes" class="card overflow-hidden">
      {#each accounts.list as a (a.id)}
        <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3.5 first:border-t-0">
          <span class="size-2.5 rounded-sm {statuses[a.status].dot}"></span>
          <div class="min-w-0 flex-[1_1_220px]">
            <div class="font-semibold">{a.label}</div>
            <div class="text-[12.5px] text-muted">
              {provider(a)} · watching {a.watch_folder} · {a.folder_count} folders{#if !a.can_move} · cannot move mail{/if} · {detail(a)}{#if a.last_event_at} · since {day(a.last_event_at)}, {clock(a.last_event_at)}{/if}
            </div>
          </div>
          <span class="text-[12.5px] font-semibold">{statuses[a.status].label}</span>
          {#if removing === a.id}
            <div role="alert" class="flex flex-[1_1_100%] flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
              <span>Remove {a.label}? MailRules deletes its password, folder list, contacts, activity, undo history and the rules that apply only to this mailbox. Nothing in the mailbox changes.</span>
              <span class="flex gap-2">
                <button type="button" class="btn" onclick={() => (removing = undefined)}>Cancel</button>
                <button type="button" class="btn-primary" onclick={() => remove(a.id)}>Remove mailbox</button>
              </span>
            </div>
          {:else}
            <div class="flex flex-wrap gap-2">
              {#if a.status === 'paused'}
                <button type="button" class="btn" aria-label="Resume {a.label}" onclick={() => setPaused(a.id, false)}>Resume</button>
              {:else}
                <button type="button" class="btn" aria-label="Reconnect {a.label}" onclick={() => reconnect(a.id)}>Reconnect</button>
                <button type="button" class="btn" aria-label="Pause {a.label}" onclick={() => setPaused(a.id, true)}>Pause</button>
              {/if}
              <button type="button" class="btn text-trash" aria-label="Remove {a.label}" onclick={() => (removing = a.id)}>Remove</button>
            </div>
          {/if}
        </div>
      {:else}
        <p class="px-[18px] py-3.5 text-secondary">No mailbox connected yet.</p>
      {/each}
    </section>
  {/if}

  {#if connecting}
    <Wizard onclose={() => (connecting = false)} />
  {/if}
</div>
