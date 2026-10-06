<script lang="ts">
  import Router, { link, router } from 'svelte-spa-router';
  import { close as closeEvents, open as openEvents } from './lib/api/events';
  import Toggle from './lib/components/Toggle.svelte';
  import { accounts, load as loadAccounts, statuses } from './lib/state/accounts.svelte';
  import { auth, logout, start } from './lib/state/auth.svelte';
  import { badges } from './lib/state/badges.svelte';
  import { load as loadReview } from './lib/state/review.svelte';
  import { load as loadRules, rules } from './lib/state/rules.svelte';
  import { load as loadSettings, settings, toggleDryRun } from './lib/state/settings.svelte';
  import { toast } from './lib/state/toast.svelte';
  import { routes, screens } from './routes';
  import Login from './screens/Login.svelte';

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

  const count = (path: string) => (path === '/rules' ? rules.list.length : badges[path]);
</script>

{#if auth.status === 'setup' || auth.status === 'login'}
  <Login />
{:else if auth.status === 'in'}
  <div class="flex min-h-screen flex-wrap">
    <nav aria-label="Main" class="flex flex-[1_1_240px] flex-col gap-5 border-r border-line-card px-3.5 py-5">
      <div class="flex items-center gap-2.5 px-1.5">
        <span class="grid size-7 place-items-center rounded bg-signal">
          <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <rect x="3" y="5" width="18" height="14" rx="2" /><path d="m3 7 9 6 9-6" />
          </svg>
        </span>
        <span class="text-base font-extrabold tracking-[-0.035em]">MailRules</span>
      </div>
      <div class="flex flex-wrap gap-0.5">
        {#each screens as s (s.path)}
          {@const on = router.location === s.path}
          <a
            href={s.path}
            use:link
            aria-current={on ? 'page' : undefined}
            class="flex min-h-10 flex-[1_1_160px] items-center gap-2.5 rounded px-2.5 no-underline {on ? 'bg-selected font-semibold text-ink' : 'font-medium text-nav'}"
          >
            <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
              <path d={s.icon} />
            </svg>
            {s.label}
            {#if count(s.path)}
              <span class="ml-auto font-mono text-xs text-muted">{count(s.path)}</span>
            {/if}
          </a>
        {/each}
      </div>
      <div class="mt-auto flex flex-col gap-2.5 px-1.5">
        <div class="label">Mailboxes</div>
        {#each accounts.list as a (a.id)}
          <div class="flex items-center gap-2">
            <span class="size-2 rounded-full {statuses[a.status].dot}"></span>
            <span class="min-w-0 flex-1 truncate font-mono text-xs">{a.label}</span>
            <span class="text-xs text-muted">{statuses[a.status].label}</span>
          </div>
        {/each}
        <div class="border-t border-line-divider"></div>
        <div class="flex items-center justify-between">
          <span>Dry-run</span>
          <Toggle on={settings.value.dry_run} label="Dry-run" onchange={toggleDryRun} />
        </div>
        <div class="flex items-center justify-between gap-2">
          <span class="min-w-0 truncate text-xs text-muted">{auth.user?.email}</span>
          <button type="button" class="border-0 bg-transparent p-0 text-xs underline" onclick={logout}>Sign out</button>
        </div>
      </div>
    </nav>

    <main class="min-w-0 flex-[999_1_480px] p-6">
      <div class="mx-auto flex max-w-[1280px] flex-col gap-[22px]">
        {#if settings.value.dry_run}
          <div role="status" class="flex flex-wrap items-center justify-between gap-2.5 rounded-md border border-warn-line bg-warn-bg px-4 py-3 text-warn">
            <span><strong>Dry-run is on.</strong> MailRules logs what it would do but doesn't touch your mailbox.</span>
            <button type="button" class="min-h-9 rounded border border-warn-strong bg-surface px-3 font-semibold text-warn" onclick={toggleDryRun}>Go live</button>
          </div>
        {/if}
        <Router {routes} />
      </div>
    </main>
  </div>
{/if}

<div aria-live="polite" class="pointer-events-none fixed inset-x-0 bottom-6 flex justify-center">
  {#if toast.text}
    <div class="rounded bg-ink px-4 py-2.5 text-surface">{toast.text}</div>
  {/if}
</div>
