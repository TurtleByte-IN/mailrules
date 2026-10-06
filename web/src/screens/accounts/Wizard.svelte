<script lang="ts">
  import { accounts, loadPresets, testSummary } from '../../lib/state/accounts.svelte';
  import { secretLabel, stepNames, Wizard } from './connect.svelte';

  let { onclose }: { onclose: () => void } = $props();

  const w = new Wizard();
  const apple = $derived(w.presetId === 'icloud');
  const chosen = $derived(w.templates.filter((t) => t.on));
  if (!accounts.presets.length) loadPresets();
</script>

{#snippet fieldError(path: string)}
  {#if w.errorField === path}
    <span role="alert" class="text-[13px] text-trash">{w.error}</span>
  {/if}
{/snippet}

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
        {#each accounts.presets as p (p.name)}
          {@const on = w.presetId === p.name}
          <button
            type="button"
            aria-pressed={on}
            onclick={() => (w.presetId = p.name)}
            class="min-h-[72px] rounded-md px-3.5 py-3 text-left {on ? 'border-2 border-ink bg-selected-row' : 'border border-line-card bg-surface'}"
          >
            <div class="font-semibold">{p.label}</div>
            <div class="text-[12.5px] text-muted">{p.host ? secretLabel(p) : 'Host, port, password'}</div>
          </button>
        {/each}
      </div>
      {#if accounts.presetsError}
        <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
          <span>Could not load the provider list. {accounts.presetsError}</span>
          <button type="button" class="btn" onclick={loadPresets}>Retry</button>
        </div>
      {/if}
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
          <span class="text-[13px] font-semibold">{w.preset?.host ? w.preset.label + ' email address' : 'Email address or username'}</span>
          <input class="field h-11" type={w.preset?.host ? 'email' : 'text'} autocomplete="off" placeholder={apple ? 'you@icloud.com' : 'you@example.com'} aria-invalid={w.errorField === 'username'} bind:value={w.email} oninput={() => w.edited()} />
          {@render fieldError('username')}
        </label>
        {#if w.preset && !w.preset.host}
          <div class="flex flex-wrap gap-3">
            <label class="flex flex-[3_1_180px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Host</span>
              <input class="field h-11 font-mono" autocomplete="off" aria-invalid={w.errorField === 'host'} bind:value={w.host} oninput={() => w.edited()} />
              {@render fieldError('host')}
            </label>
            <label class="flex flex-[2_1_150px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Encryption</span>
              <select class="field h-11 px-2.5" aria-invalid={w.errorField === 'tls_mode'} value={w.tls} onchange={(e) => w.setTls(e.currentTarget.value as typeof w.tls)}>
                <option value="implicit">TLS (port 993)</option>
                <option value="starttls">STARTTLS (port 143)</option>
              </select>
              {@render fieldError('tls_mode')}
            </label>
            <label class="flex flex-[1_1_80px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Port</span>
              <input class="field h-11 font-mono" type="number" min="1" max="65535" aria-invalid={w.errorField === 'port'} bind:value={w.port} oninput={() => w.portEdited()} />
              {@render fieldError('port')}
            </label>
          </div>
        {/if}
        <label class="flex flex-col gap-1.5">
          <span class="text-[13px] font-semibold">{w.preset ? secretLabel(w.preset) : ''}</span>
          <input class="field h-11 font-mono" type="password" autocomplete="off" placeholder={apple ? 'xxxx-xxxx-xxxx-xxxx' : undefined} aria-invalid={w.errorField === 'password'} bind:value={w.password} oninput={() => w.edited()} />
          {@render fieldError('password')}
        </label>
        <button class="btn min-h-11 font-semibold" disabled={w.test === 'testing'}>{w.testLabel}</button>
        {#if w.test === 'ok' && w.result}
          <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]">{testSummary(w.result)}</div>
        {:else if w.test === 'err' && !w.errorField}
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
      <h2>Starter rules you picked</h2>
      {#each chosen as t (t.id)}
        <div class="rounded-md border border-line-card px-3.5 py-3"><span class="font-semibold">{t.name}</span><span class="text-secondary"> · {t.desc}</span></div>
      {:else}
        <p class="text-secondary">No starter rules chosen. You can add rules any time.</p>
      {/each}
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
