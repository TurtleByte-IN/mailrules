<script lang="ts">
  import { link, router } from 'svelte-spa-router';
  import { screens } from '../../routes';
  import { accounts, statuses } from '../state/accounts.svelte';
  import { auth, logout } from '../state/auth.svelte';
  import { badges } from '../state/badges.svelte';
  import { rules } from '../state/rules.svelte';
  import { settings, toggleDryRun } from '../state/settings.svelte';
  import Toggle from './Toggle.svelte';

  // Below md the nav is a bar with a Menu button that opens the rest; from md up it is the sidebar.
  let open = $state(false);
  const count = (path: string) => (path === '/rules' ? rules.list.length : badges[path]);
</script>

<svelte:window onkeydown={(e) => e.key === 'Escape' && (open = false)} />

<nav
  aria-label="Main"
  class="flex flex-[1_1_240px] flex-col gap-5 border-r border-line-card px-3.5 py-5 max-md:sticky max-md:top-0 max-md:z-10 max-md:max-h-dvh max-md:flex-none max-md:gap-3 max-md:overflow-y-auto max-md:border-r-0 max-md:border-b max-md:bg-surface max-md:py-2"
>
  <div class="flex items-center gap-2.5 px-1.5">
    <span class="grid size-7 place-items-center rounded bg-signal">
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="#fff" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
        <rect x="3" y="5" width="18" height="14" rx="2" /><path d="m3 7 9 6 9-6" />
      </svg>
    </span>
    <span class="text-base font-extrabold tracking-[-0.035em]">MailRules</span>
    <button type="button" class="btn ml-auto md:hidden" aria-expanded={open} aria-controls="nav-menu" onclick={() => (open = !open)}>
      <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" aria-hidden="true">
        <path d={open ? 'M18 6 6 18M6 6l12 12' : 'M4 7h16M4 12h16M4 17h16'} />
      </svg>
      Menu
    </button>
  </div>
  <div id="nav-menu" class="flex flex-1 flex-col gap-5 max-md:pb-3 {open ? '' : 'max-md:hidden'}">
    <div class="flex flex-wrap gap-0.5">
      {#each screens as s (s.path)}
        {@const on = router.location === s.path}
        <a
          href={s.path}
          use:link
          aria-current={on ? 'page' : undefined}
          onclick={() => (open = false)}
          class="flex min-h-10 flex-[1_1_160px] items-center gap-2.5 rounded px-2.5 no-underline max-md:min-h-11 {on ? 'bg-selected font-semibold text-ink' : 'font-medium text-nav'}"
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
        <button type="button" class="border-0 bg-transparent p-0 text-xs underline max-md:min-h-11" onclick={logout}>Sign out</button>
      </div>
    </div>
  </div>
</nav>
