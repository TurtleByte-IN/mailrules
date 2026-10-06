<script lang="ts">
  import type { Account } from '../lib/api/accounts';
  import { clock, day } from '../lib/format';
  import { accounts, reconnect, remove, test } from '../lib/state/accounts.svelte';
  import Wizard from './accounts/Wizard.svelte';

  let connecting = $state(false);
  let removing = $state('');

  const dot = { live: 'bg-live', reconnecting: 'bg-warn-strong', error: 'bg-trash' };
  const detail = (a: Account) =>
    a.status === 'live' ? 'push (IDLE) connected' : a.status === 'error' ? (a.lastError ?? 'error') : 'reconnecting';
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

  <section aria-label="Connected mailboxes" class="card overflow-hidden">
    {#each accounts.list as a (a.id)}
      <div class="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-line-divider px-[18px] py-3.5 first:border-t-0">
        <span class="size-2.5 rounded-sm {dot[a.status]}"></span>
        <div class="min-w-0 flex-[1_1_220px]">
          <div class="font-semibold">{a.email}</div>
          <div class="text-[12.5px] text-muted">
            {a.provider} · {a.folders} folders · {detail(a)}{#if a.lastEventAt} · last event {day(a.lastEventAt)}, {clock(a.lastEventAt)}{/if}
          </div>
        </div>
        <span class="text-[12.5px] font-semibold capitalize">{a.status}</span>
        {#if removing === a.id}
          <div role="alert" class="flex flex-[1_1_100%] flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
            <span>Remove {a.email}? MailRules forgets its password, activity and undo history. Nothing in the mailbox changes.</span>
            <span class="flex gap-2">
              <button type="button" class="btn" onclick={() => (removing = '')}>Cancel</button>
              <button type="button" class="btn-primary" onclick={() => remove(a.id)}>Remove mailbox</button>
            </span>
          </div>
        {:else}
          <div class="flex flex-wrap gap-2">
            <button type="button" class="btn" aria-label="Test {a.email}" onclick={() => test(a.id)}>Test</button>
            <button type="button" class="btn" aria-label="Reconnect {a.email}" onclick={() => reconnect(a.id)}>Reconnect</button>
            <button type="button" class="btn text-trash" aria-label="Remove {a.email}" onclick={() => (removing = a.id)}>Remove</button>
          </div>
        {/if}
      </div>
    {:else}
      <p class="px-[18px] py-3.5 text-secondary">No mailbox connected yet.</p>
    {/each}
  </section>

  {#if connecting}
    <Wizard onclose={() => (connecting = false)} />
  {/if}
</div>
