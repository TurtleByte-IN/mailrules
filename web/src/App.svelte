<script lang="ts">
  import Router, { replace } from 'svelte-spa-router';
  import { close as closeEvents, open as openEvents } from './lib/api/events';
  import Nav from './lib/components/Nav.svelte';
  import { accounts, load as loadAccounts } from './lib/state/accounts.svelte';
  import { auth, start } from './lib/state/auth.svelte';
  import { firstRun, settle } from './lib/state/firstrun.svelte';
  import { setupReturn } from './lib/state/oneclick';
  import { load as loadReview } from './lib/state/review.svelte';
  import { load as loadRules } from './lib/state/rules.svelte';
  import { load as loadSettings, settings, toggleDryRun } from './lib/state/settings.svelte';
  import { toast } from './lib/state/toast.svelte';
  import { routes } from './routes';
  import Login from './screens/Login.svelte';

  // A one-click sign-in started from first-run setup comes back to Mailboxes like any other:
  // the guide takes it up again at its mailbox step, before the screens are drawn.
  const resume = setupReturn(location.hash);
  if (resume) {
    firstRun.step = 'mailbox';
    replace(resume.slice(1));
  }

  start();

  $effect(() => {
    if (auth.status === 'in') {
      openEvents();
      loadAccounts();
      loadRules();
      loadReview();
      loadSettings();
    } else {
      closeEvents();
    }
  });

  // Right after the admin account is created, the first-run guide opens once the mailboxes
  // are listed, unless there already is one.
  $effect(() => {
    if (firstRun.offered && accounts.loaded) settle(accounts.list.length);
  });
</script>

{#if auth.status === 'setup' || auth.status === 'login'}
  <Login />
{:else if auth.status === 'in'}
  <div class="flex min-h-screen flex-wrap max-md:flex-col max-md:flex-nowrap">
    <Nav />

    <main class="min-w-0 flex-[999_1_480px] p-6 max-md:p-4">
      <div class="mx-auto flex max-w-[1280px] flex-col gap-[22px]">
        {#if settings.value.dry_run}
          <div role="status" class="flex flex-wrap items-center justify-between gap-2.5 rounded-md border border-warn-line bg-warn-bg px-4 py-3 text-warn">
            <span><strong>Dry-run is on.</strong> MailRules logs what it would do but doesn't touch your mailbox.</span>
            <button type="button" class="min-h-9 rounded border border-warn-strong bg-surface px-3 font-semibold text-warn max-md:min-h-11" onclick={toggleDryRun}>Go live</button>
          </div>
        {/if}
        <Router {routes} />
      </div>
    </main>
  </div>
{/if}

<div aria-live="polite" class="pointer-events-none fixed inset-x-0 bottom-6 flex justify-center px-4">
  {#if toast.text}
    <div class="rounded bg-ink px-4 py-2.5 text-surface">{toast.text}</div>
  {/if}
</div>
