<script lang="ts">
  import Router, { link, router } from 'svelte-spa-router';
  import { routes, screens } from './routes';
</script>

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
        </a>
      {/each}
    </div>
  </nav>

  <main class="min-w-0 flex-[999_1_480px] p-6">
    <Router {routes} />
  </main>
</div>
