<script lang="ts">
  import Waiting from '../../lib/components/Waiting.svelte';
  import { accounts, loadPresets, testSummary } from '../../lib/state/accounts.svelte';
  import CertCheck from './CertCheck.svelte';
  import { secretLabel, stepNames, Wizard, zohoRegions } from './connect.svelte';

  /**
   * `onclose` runs when the first step's `cancelLabel` button is pressed, and once the mailbox
   * is saved unless `onconnected` is given. `added` is a mailbox a one-click sign-in just
   * connected: the wizard opens at Rules for it. `from` is the screen a one-click sign-in
   * started here comes back to.
   */
  let {
    onclose,
    onconnected,
    cancelLabel = 'Cancel',
    added,
    from = 'accounts',
  }: { onclose: () => void; onconnected?: () => void; cancelLabel?: string; added?: number; from?: 'setup' | 'accounts' } = $props();

  // svelte-ignore state_referenced_locally -- read once: the wizard is made again for another mailbox
  const w = new Wizard(added, from);
  const apple = $derived(w.presetId === 'icloud');
  const proton = $derived(w.presetId === 'proton');
  const chosen = $derived(w.templates.filter((t) => t.on));
  // A provider a module signs in to has a one-click tile, ahead of its app-password tile when it takes one.
  const tiles = $derived(accounts.presets.flatMap((p) => [...(p.one_click_url ? [{ p, oneClick: true }] : []), ...(p.password ? [{ p, oneClick: false }] : [])]));
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
        {#each tiles as { p, oneClick } (p.name + (oneClick ? ' one-click' : ''))}
          {@const on = w.presetId === p.name && w.oneClick === oneClick}
          <button
            type="button"
            aria-pressed={on}
            onclick={() => w.choose(p.name, oneClick)}
            class="min-h-[72px] rounded-md px-3.5 py-3 text-left {on ? 'border-2 border-ink bg-selected-row' : 'border border-line-card bg-surface'}"
          >
            <div class="font-semibold">{p.label}</div>
            <div class="text-[12.5px] text-muted">{oneClick ? 'One-click sign-in' : p.host ? secretLabel(p) : 'Host, port, password'}</div>
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
      {#if proton}
        <div class="flex flex-[1_1_300px] flex-col gap-2.5 rounded-md border border-selected bg-selected-row p-3.5">
          <div class="font-semibold">Connect through Proton Mail Bridge</div>
          <ol class="m-0 flex list-decimal flex-col gap-1.5 pl-[18px] text-[13.5px] text-nav">
            <li>Proton Mail Bridge needs a paid Proton plan. Install it on this machine, the one MailRules runs on, sign in, and leave it running.</li>
            <li>In Bridge, open your account's Mailbox details and copy the IMAP username, password and port.</li>
            <li>Bridge makes its own certificate. After Test connection, MailRules shows its fingerprint for you to accept.</li>
          </ol>
          <a href={w.preset?.help_url} target="_blank" rel="noreferrer" class="text-[13.5px] font-semibold">How to set up Proton Mail Bridge</a>
          <div class="text-[12.5px] text-secondary">MailRules in Docker cannot reach a Bridge running on this machine. Use the binary or the Homebrew install.</div>
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
        {#if w.presetId === 'zoho'}
          <label class="flex flex-col gap-1.5">
            <span class="text-[13px] font-semibold">Where is your Zoho account?</span>
            <select class="field h-11 px-2.5" aria-invalid={w.errorField === 'host'} bind:value={w.region} onchange={() => w.serverEdited()}>
              {#each zohoRegions as r (r.id)}
                <option value={r.id}>{r.label}</option>
              {/each}
            </select>
            <span class="text-[12.5px] text-secondary">Use the address you see when you sign in to Zoho Mail. Paid organisations may need Other IMAP server with imappro.zoho.com.</span>
            {@render fieldError('host')}
          </label>
        {/if}
        {#if w.serverFields}
          <div class="flex flex-wrap gap-3">
            <label class="flex flex-[3_1_180px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Host</span>
              <input class="field h-11 font-mono" autocomplete="off" aria-invalid={w.errorField === 'host'} bind:value={w.host} oninput={() => w.serverEdited()} />
              {@render fieldError('host')}
            </label>
            <label class="flex flex-[2_1_150px] flex-col gap-1.5">
              <span class="text-[13px] font-semibold">Encryption</span>
              <select class="field h-11 px-2.5" aria-invalid={w.errorField === 'tls_mode'} value={w.tls} onchange={(e) => w.setTls(e.currentTarget.value as typeof w.tls)}>
                <option value="implicit">{proton ? 'SSL' : 'TLS (port 993)'}</option>
                <option value="starttls">{proton ? 'STARTTLS' : 'STARTTLS (port 143)'}</option>
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
        {#if w.test === 'testing'}
          <Waiting text="Logging in to your mail server and listing its folders" />
        {:else if w.test === 'ok' && w.result}
          <div role="status" class="rounded bg-selected px-3 py-2.5 text-[13px]">{testSummary(w.result)}</div>
        {:else if w.test === 'err' && w.cert}
          <CertCheck cert={w.cert} message={w.error} onaccept={() => w.acceptCert()} />
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
      {#if w.busy}
        <Waiting text={w.accountId ? 'Saving the rules you picked' : 'Connecting your mailbox and saving the rules you picked'} />
      {/if}
      {#each chosen as t (t.id)}
        <div class="rounded-md border border-line-card px-3.5 py-3"><span class="font-semibold">{t.name}</span><span class="text-secondary"> · {t.desc}</span></div>
      {:else}
        <p class="text-secondary">No starter rules chosen. You can add rules any time.</p>
      {/each}
      {#if w.accountId}
        <p class="text-[13px] text-secondary">Nothing has moved yet. Your mailbox is connected and MailRules is watching it, but dry-run stays on: new mail is previewed in Activity, and nothing moves until you turn dry-run off. Existing mail stays put until you run Cleanup.</p>
      {:else}
        <p class="text-[13px] text-secondary">Nothing has moved yet. Connect saves the mailbox and starts watching it, but dry-run stays on: new mail is previewed in Activity, and nothing moves until you turn dry-run off. Existing mail stays put until you run Cleanup.</p>
      {/if}
    </div>
  {/if}

  <div class="flex flex-wrap justify-between gap-2 border-t border-line-divider pt-3.5">
    <button type="button" class="btn min-h-11" onclick={() => (w.step === w.first ? onclose() : w.step--)}>{w.step === w.first ? cancelLabel : 'Back'}</button>
    <button
      type="button"
      class="btn-primary min-h-11 px-[18px]"
      disabled={w.busy || w.test === 'testing' || !w.preset}
      onclick={async () => {
        if (await w.next()) (onconnected ?? onclose)();
      }}
    >
      {w.nextLabel}
    </button>
  </div>
</section>
