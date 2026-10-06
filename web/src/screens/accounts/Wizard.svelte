<script lang="ts">
  import { accounts, loadPresets, testSummary } from '../../lib/state/accounts.svelte';
  import { preview, stepNames, Wizard } from './connect.svelte';

  let { onclose }: { onclose: () => void } = $props();

  const w = new Wizard();
  const apple = $derived(w.presetId === 'icloud');
  loadPresets();
</script>

<section aria-label="Add a mailbox" class="card flex flex-col gap-[18px] p-5">
  <ol class="m-0 flex list-none flex-wrap gap-2 p-0">
    {#each stepNames as label, i (label)}
      <li
        aria-current={i === w.step ? 'step' : undefined}
        class="flex items-center gap-2 rounded py-1.5 pr-3 pl-1.5 text-[13px] {i === w.step ? 'bg-selected font-semibold' : 'font-medium'} {i <= w.step ? 'text-ink' : 'text-muted'}"
      >
        <span class="inline-flex size-[22px] items-center justify-center rounded-sm text-xs {i <= w.step ? 'bg-ink text-surface' : 'bg-neutral text-secondary'}">{i + 1}</span>
        {label}
      </li>
    {/each}
  </ol>

  {#if w.step === 0}
    <div class="flex flex-col gap-3">
      <h2>Where is your email?</h2>
      <div class="grid grid-cols-[repeat(auto-fit,minmax(160px,1fr))] gap-2.5">
        {#each accounts.presets as p (p.id)}
          {@const on = w.presetId === p.id}
          <button
            type="button"
            disabled={!p.available}
            aria-pressed={on}
            onclick={() => (w.presetId = p.id)}
            class="min-h-[72px] rounded-md px-3.5 py-3 text-left {on ? 'border-2 border-ink bg-selected-row' : 'border border-line-card bg-surface'}"
          >
            <div class="font-semibold">{p.name}</div>
            <div class="text-[12.5px] text-muted">{p.note}</div>
          </button>
        {/each}
      </div>
    </div>
  {:else if w.step === 1}
    <div class="flex flex-wrap gap-5">
      {#if apple}
        <div class="flex flex-[1_1_300px] flex-col gap-2.5 rounded-md border border-selected bg-selected-row p-3.5">
          <div class="font-semibold">Create an app-specific password</div>
          <ol class="m-0 flex list-decimal flex-col gap-1.5 pl-[18px] text-[13.5px] text-nav">
            <li>Open account.apple.com and sign in.</li>
            <li>Go to Sign-In and Security, then App-Specific Passwords.</li>
            <li>Click the plus button, name it MailRules, and copy the password.</li>
          </ol>
          <a href="https://account.apple.com" target="_blank" rel="noreferrer" class="text-[13.5px] font-semibold">Open account.apple.com</a>
          <div class="text-[12.5px] text-secondary">MailRules never sees your Apple ID password. You can revoke this one any time.</div>
        </div>
      {/if}
      <form
        class="flex flex-[1_1_300px] flex-col gap-3"
        onsubmit={(e) => {
          e.preventDefault();
          w.runTest();
        }}
      >
        <label class="flex flex-col gap-1.5">
          <span class="text-[13px] font-semibold">{w.preset?.name} email address</span>
          <input class="field h-11" type="email" autocomplete="off" placeholder={apple ? 'you@icloud.com' : 'you@example.com'} bind:value={w.email} oninput={() => w.edited()} />
        </label>
        {#if w.preset && !w.preset.knowsHost}
          <div class="flex flex-wrap gap-3">
            <label class="flex flex-[3_1_180px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Host</span>
              <input class="field h-11 font-mono" autocomplete="off" bind:value={w.host} oninput={() => w.edited()} />
            </label>
            <label class="flex flex-[1_1_80px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Port</span>
              <input class="field h-11 font-mono" type="number" min="1" max="65535" bind:value={w.port} oninput={() => w.edited()} />
            </label>
          </div>
        {/if}
        <label class="flex flex-col gap-1.5">
          <span class="text-[13px] font-semibold">{w.preset?.secretLabel}</span>
          <input class="field h-11 font-mono" type="password" autocomplete="off" placeholder={apple ? 'xxxx-xxxx-xxxx-xxxx' : undefined} bind:value={w.password} oninput={() => w.edited()} />
        </label>
        <button class="btn min-h-11 font-semibold" disabled={w.test === 'testing'}>{w.testLabel}</button>
        {#if w.test === 'ok' && w.result}
          <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]">{testSummary(w.result)}</div>
        {:else if w.test === 'err'}
          <div role="alert" class="rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">{w.error}</div>
        {/if}
      </form>
    </div>
  {:else if w.step === 2}
    <div class="flex flex-col gap-2.5">
      <h2>Start with a few rules</h2>
      <p class="text-secondary">Pick any; you can edit them or add your own in plain words later.</p>
      {#each w.templates as t (t.id)}
        <label class="flex cursor-pointer items-center gap-3 rounded-md border border-line-card px-3.5 py-3">
          <input type="checkbox" bind:checked={t.on} />
          <span class="flex-1"><span class="font-semibold">{t.name}</span><span class="text-secondary"> · {t.desc}</span></span>
        </label>
      {/each}
    </div>
  {:else}
    <div class="flex flex-col gap-3">
      <h2>Here's what would happen to your last 100 emails</h2>
      <div class="grid grid-cols-[repeat(auto-fit,minmax(150px,1fr))] gap-2.5">
        {#each preview as pv (pv.label)}
          <div class="rounded-md border border-line-card p-3.5">
            <div class="text-2xl font-semibold">{pv.count}</div>
            <div class="text-[13px] text-secondary">{pv.label}</div>
          </div>
        {/each}
      </div>
      <p class="text-[13px] text-secondary">Nothing has moved yet. Go live to start sorting new mail; existing mail stays put until you run Cleanup.</p>
    </div>
  {/if}

  <div class="flex flex-wrap justify-between gap-2 border-t border-line-divider pt-3.5">
    <button type="button" class="btn min-h-11" onclick={() => (w.step === 0 ? onclose() : w.step--)}>{w.step === 0 ? 'Cancel' : 'Back'}</button>
    <button
      type="button"
      class="btn-primary min-h-11 px-[18px]"
      disabled={w.busy || w.test === 'testing' || !w.preset}
      onclick={async () => {
        if (await w.next()) onclose();
      }}
    >
      {w.nextLabel}
    </button>
  </div>
</section>
