<script lang="ts">
  import { onMount } from 'svelte';
  import { replace } from 'svelte-spa-router';
  import type { Decider } from '../lib/api/settings';
  import { close, firstRun, next, skip, stepNames } from '../lib/state/firstrun.svelte';
  import { findWorkspace, load, settings } from '../lib/state/settings.svelte';
  import Wizard from './accounts/Wizard.svelte';
  import DeciderFields from './settings/DeciderFields.svelte';
  import KeyFields from './settings/KeyFields.svelte';
  import { credentials, keyOrder } from './settings/keysInUse';

  const s = $derived(settings.value);
  let decider = $state<Decider>(settings.value.decider);
  // The keys the picked decider needs, then any other a warning names (the rule composer's).
  const keys = $derived([
    ...credentials[decider],
    ...keyOrder.filter((k) => !credentials[decider].includes(k) && s.warnings.some((w) => w.path === 'keys.' + k)),
  ]);
  const ready = $derived(!s.warnings.some((w) => w.code === 'decider_not_ready'));
  const current = $derived(firstRun.step === 'ai' ? 1 : firstRun.step === 'mailbox' ? 2 : 3);

  // The guide only exists right after the admin account was created; opened any other way
  // (a reload, a typed link) it gives way to Overview. Closing it opens a screen of its own.
  onMount(async () => {
    if (!firstRun.step) return replace('/');
    if (!settings.loaded) await load();
    await findWorkspace();
  });
</script>

{#if firstRun.step}
  <div class="flex max-w-[760px] flex-col gap-[18px]">
    <header class="flex flex-wrap items-start justify-between gap-3">
      <div>
        <h1>Welcome to MailRules</h1>
        <p class="mt-1 text-secondary">Your admin account is ready. Two more steps, and you can skip either.</p>
      </div>
      {#if firstRun.step !== 'done'}
        <button type="button" class="btn min-h-11" onclick={() => close('/')}>Skip setup</button>
      {/if}
    </header>

    <ol class="m-0 flex list-none flex-wrap gap-2 p-0">
      {#each stepNames as label, i (label)}
        <li
          aria-current={i === current ? 'step' : undefined}
          class="flex items-center gap-2 rounded py-1.5 pr-3 pl-1.5 text-[13px] {i === current ? 'bg-selected font-semibold' : 'font-medium'} {i <= current ? 'text-ink' : 'text-muted'}"
        >
          <span class="inline-flex size-[22px] items-center justify-center rounded-sm text-xs {i <= current ? 'bg-ink text-surface' : 'bg-neutral text-secondary'}">{i + 1}</span>
          {label}
        </li>
      {/each}
    </ol>

    {#if firstRun.step === 'ai'}
      <section aria-labelledby="setup-ai" class="card flex flex-col gap-[18px] p-5">
        <div>
          <h2 id="setup-ai">Choose how MailRules decides</h2>
          <p class="mt-1 text-[13.5px] text-secondary">Rules written in plain words need an AI to read your mail. Rules made only of conditions work without one.</p>
        </div>
        {#if settings.error}
          <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-[18px] py-3 text-trash">
            <span>Could not load the settings. {settings.error}</span>
            <button type="button" class="btn" onclick={load}>Retry</button>
          </div>
        {/if}
        {#if settings.loaded}
          <DeciderFields bind:chosen={decider} />
          <KeyFields shown={keys} />
          <p class="text-[12.5px] text-secondary">Keys are encrypted with your master key and never logged. You can change all of this later in Settings.</p>
        {/if}
        <div class="flex flex-wrap justify-between gap-2 border-t border-line-divider pt-3.5">
          <button type="button" class="btn min-h-11" onclick={skip}>Skip for now</button>
          <button type="button" class="btn-primary min-h-11 px-[18px]" disabled={!settings.loaded || !ready} onclick={next}>Continue</button>
        </div>
      </section>
    {:else if firstRun.step === 'mailbox'}
      <div>
        <h2>Connect your first mailbox</h2>
        <p class="mt-1 text-secondary">Pick your provider, sign in with an app password and test the connection. Dry-run stays on: nothing in your mailbox changes until you switch it off.</p>
      </div>
      <Wizard cancelLabel="Skip for now" onclose={skip} onconnected={next} />
    {:else}
      <section aria-labelledby="setup-done" class="card flex flex-col gap-3.5 p-5">
        <h2 id="setup-done">You're set</h2>
        <p class="text-secondary">MailRules is watching your mailbox in dry-run. Check Activity for a day, then switch dry-run off.</p>
        <div class="flex justify-end border-t border-line-divider pt-3.5">
          <button type="button" class="btn-primary min-h-11 px-[18px]" onclick={() => close('/activity')}>Open Activity</button>
        </div>
      </section>
    {/if}
  </div>
{/if}
