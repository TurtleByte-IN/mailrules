<script lang="ts">
  import { tick } from 'svelte';
  import type { Account, AccountPatch, ServerCert } from '../lib/api/accounts';
  import { ApiError } from '../lib/api/client';
  import { clock, day } from '../lib/format';
  import { acceptCert, accounts, checkCert, edit, folderNames, load, loadPresets, reconnect, remove, setPaused, statuses, test } from '../lib/state/accounts.svelte';
  import { auth } from '../lib/state/auth.svelte';
  import { flash } from '../lib/state/toast.svelte';
  import Toggle from '../lib/components/Toggle.svelte';
  import CertCheck from './accounts/CertCheck.svelte';
  import { editableServer, zohoRegions } from './accounts/connect.svelte';
  import Wizard from './accounts/Wizard.svelte';

  let connecting = $state(false);
  let removing = $state<number>();
  let editing = $state<number>();
  // Joined here, not in the markup: a separator at the start of an {#if} block loses its leading space.
  const line = (a: Account) =>
    [
      provider(a),
      'watching ' + a.watch_folder,
      a.folder_count + ' folders',
      !a.can_move && 'cannot move mail',
      detail(a),
      a.last_event_at && 'since ' + day(a.last_event_at) + ', ' + clock(a.last_event_at),
      a.last_mail_at && 'last email ' + day(a.last_mail_at) + ', ' + clock(a.last_mail_at),
    ]
      .filter(Boolean)
      .join(' · ');

  let testing = $state<Record<number, boolean>>({});
  // A changed server certificate being checked: 'checking' while the daemon asks the server, then what it presents.
  let certs = $state<Record<number, 'checking' | { cert: ServerCert; message: string; busy: boolean }>>({});
  // The Edit form of the mailbox being edited. A new password lives here only, until it is sent or the form closes.
  let form = $state({
    label: '', folder: '', password: '', shared: false, host: '', port: 0, folders: [] as string[], error: '', errorPath: '', busy: false,
    /** The new server's certificate, while the person is asked to accept it. */
    cert: undefined as ServerCert | undefined,
  });
  // The edit refused for the certificate above, sent again with it once accepted. It holds a new password, if one
  // was typed, until then or until the form closes.
  let pending: AccountPatch | undefined;
  const editFields = ['label', 'watch_folder', 'password', 'host', 'port'];

  // The list names each provider by its preset label.
  if (!accounts.presets.length) loadPresets();

  const provider = (a: Account) => accounts.presets.find((p) => p.name === a.preset)?.label ?? a.host;
  const detail = (a: Account) =>
    a.status === 'live'
      ? a.capabilities.includes('IDLE') ? 'push (IDLE) connected' : 'checked once a minute'
      : a.status === 'cert_changed' ? '' : a.last_error || statuses[a.status].label.toLowerCase();

  async function runTest(id: number) {
    testing[id] = true;
    await test(id);
    testing[id] = false;
  }

  async function reviewCert(id: number) {
    certs[id] = 'checking';
    const found = await checkCert(id);
    if (found) certs[id] = { ...found, busy: false };
    else delete certs[id];
  }

  async function accept(id: number) {
    const c = certs[id];
    if (typeof c !== 'object') return;
    c.busy = true;
    await acceptCert(id, c.cert.fingerprint);
    delete certs[id];
  }

  /**
   * What of the server Edit can change, as the wizard asked for it: host and port for a server the person
   * named (Other IMAP server, Proton Mail Bridge), the region's host for Zoho, nothing for a provider's own.
   */
  const serverEdit = (a: Account) => (a.preset === 'zoho' ? 'region' : a.preset === 'generic' || editableServer.includes(a.preset) ? 'server' : '');

  async function openEdit(a: Account, focus: 'name' | 'password') {
    form = { label: a.label, folder: a.watch_folder, password: '', shared: a.shared, host: a.host, port: a.port, folders: [a.watch_folder], error: '', errorPath: '', busy: false, cert: undefined };
    pending = undefined;
    editing = a.id;
    await tick();
    document.getElementById('edit-' + focus)?.focus();
    const names = await folderNames(a.id);
    if (editing === a.id && names.length) form.folders = names.includes(a.watch_folder) ? names : [a.watch_folder, ...names];
  }

  function closeEdit() {
    editing = undefined;
    form.password = '';
    form.cert = pending = undefined;
  }

  /** Sends only what changed; an empty password field means the stored one stays. */
  async function save(e: SubmitEvent, a: Account) {
    e.preventDefault();
    const p: AccountPatch = {};
    if (form.label.trim() !== a.label) p.label = form.label.trim();
    if (form.folder !== a.watch_folder) p.watch_folder = form.folder;
    if (form.password.trim()) p.password = form.password;
    if (form.shared !== a.shared) p.shared = form.shared;
    if (form.host.trim() !== a.host) p.host = form.host.trim();
    if (form.port !== a.port) p.port = form.port;
    form.password = '';
    if (!Object.keys(p).length) return closeEdit();
    await send(a.id, p);
  }

  /** A new server is tested before it is saved: a refusal leaves the mailbox as it was and the form open. */
  async function send(id: number, p: AccountPatch) {
    form.busy = true;
    form.errorPath = '';
    form.cert = pending = undefined;
    try {
      await edit(id, p);
      closeEdit();
    } catch (err) {
      if (err instanceof ApiError && err.cert) [form.cert, form.error, pending] = [err.cert, err.message, p];
      else if (err instanceof ApiError && err.path && editFields.includes(err.path)) [form.error, form.errorPath] = [err.message, err.path];
      else flash(err instanceof Error ? err.message : String(err));
    } finally {
      form.busy = false;
    }
  }
</script>

{#snippet fieldError(path: string)}
  {#if form.errorPath === path}
    <span role="alert" class="text-[13px] text-trash">{form.error}</span>
  {/if}
{/snippet}

<div class="flex max-w-[920px] flex-col gap-[18px]">
  <header class="flex flex-wrap items-end justify-between gap-4">
    <div>
      <h1>Mailboxes</h1>
      <p class="mt-1 text-secondary">Any IMAP mailbox that signs in with a password or app password. MailRules sorts on the server, so every app you use sees the result.</p>
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
              {line(a)}
            </div>
          </div>
          <span class="text-[12.5px] font-semibold">{statuses[a.status].label}</span>
          {#if removing === a.id}
            <div role="alert" class="flex flex-[1_1_100%] flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
              <span>Remove {a.label}? MailRules deletes its password, folder list, contacts, activity and undo history. Rules that apply only to this mailbox are kept but switched off, marked so you can give them another mailbox. Nothing in the mailbox changes.</span>
              <span class="flex gap-2">
                <button type="button" class="btn" onclick={() => (removing = undefined)}>Cancel</button>
                <button type="button" class="btn-primary" onclick={() => remove(a.id)}>Remove mailbox</button>
              </span>
            </div>
          {:else if editing === a.id}
            <form aria-label="Edit {a.label}" class="flex flex-[1_1_100%] flex-wrap items-start gap-3 rounded bg-selected-row px-3 py-3" onsubmit={(e) => save(e, a)}>
              <label class="flex flex-[1_1_180px] flex-col gap-1.5">
                <span class="text-[13px] font-semibold">Name</span>
                <input id="edit-name" class="field h-11" autocomplete="off" aria-invalid={form.errorPath === 'label'} bind:value={form.label} />
                {@render fieldError('label')}
              </label>
              <label class="flex flex-[1_1_180px] flex-col gap-1.5">
                <span class="text-[13px] font-semibold">Watched folder</span>
                <select class="field h-11 px-2.5" aria-invalid={form.errorPath === 'watch_folder'} bind:value={form.folder}>
                  {#each form.folders as name (name)}
                    <option value={name}>{name}</option>
                  {/each}
                </select>
                {@render fieldError('watch_folder')}
              </label>
              {#if serverEdit(a) === 'server'}
                <label class="flex flex-[3_1_180px] flex-col gap-1.5">
                  <span class="text-[13px] font-semibold">Host</span>
                  <input class="field h-11 font-mono" autocomplete="off" aria-invalid={form.errorPath === 'host'} bind:value={form.host} />
                  {@render fieldError('host')}
                </label>
                <label class="flex flex-[1_1_80px] flex-col gap-1.5">
                  <span class="text-[13px] font-semibold">Port</span>
                  <input class="field h-11 font-mono" type="number" min="1" max="65535" aria-invalid={form.errorPath === 'port'} bind:value={form.port} />
                  {@render fieldError('port')}
                </label>
              {:else if serverEdit(a) === 'region'}
                <label class="flex flex-[1_1_180px] flex-col gap-1.5">
                  <span class="text-[13px] font-semibold">Zoho region</span>
                  <select class="field h-11 px-2.5" aria-invalid={form.errorPath === 'host'} bind:value={form.host}>
                    {#if !zohoRegions.some((r) => r.host === a.host)}
                      <option value={a.host}>{a.host}</option>
                    {/if}
                    {#each zohoRegions as r (r.id)}
                      <option value={r.host}>{r.label}</option>
                    {/each}
                  </select>
                  {@render fieldError('host')}
                </label>
              {/if}
              <label class="flex flex-[1_1_180px] flex-col gap-1.5">
                <span class="text-[13px] font-semibold">New app password</span>
                <input id="edit-password" class="field h-11 font-mono" type="password" autocomplete="new-password" aria-invalid={form.errorPath === 'password'} bind:value={form.password} />
                {@render fieldError('password')}
                <span class="text-[12.5px] text-secondary">Leave empty to keep the current one.</span>
              </label>
              {#if a.mine && auth.members > 1}
                <span class="flex flex-[1_1_100%] items-center gap-3">
                  <Toggle on={form.shared} label="Shared with team" onchange={() => (form.shared = !form.shared)} />
                  <span class="flex flex-col">
                    <span class="text-[13px] font-semibold">Shared with team</span>
                    <span class="text-[12.5px] text-secondary">Everyone in your team sees this mailbox and can act on its mail. Only you can edit it.</span>
                  </span>
                </span>
              {/if}
              {#if serverEdit(a)}
                <span class="flex-[1_1_100%] text-[12.5px] text-secondary">A new server is tested before it is saved; if the test fails, nothing changes.</span>
              {/if}
              {#if form.cert}
                <div class="flex-[1_1_100%]">
                  <CertCheck cert={form.cert} message={form.error} busy={form.busy} onaccept={() => pending && send(a.id, { ...pending, cert_fingerprint: form.cert!.fingerprint })} />
                </div>
              {/if}
              <span class="flex flex-[1_1_100%] justify-end gap-2">
                <button type="button" class="btn" onclick={closeEdit}>Cancel</button>
                <button class="btn-primary" disabled={form.busy}>{form.busy ? 'Saving…' : 'Save'}</button>
              </span>
            </form>
          {:else if a.mine}
            <div class="flex flex-wrap gap-2">
              {#if a.status === 'paused'}
                <button type="button" class="btn" aria-label="Resume {a.label}" onclick={() => setPaused(a.id, false)}>Resume</button>
              {:else}
                {#if a.status === 'live'}
                  <button type="button" class="btn" aria-label="Test {a.label}" disabled={testing[a.id]} onclick={() => runTest(a.id)}>{testing[a.id] ? 'Testing…' : 'Test'}</button>
                {:else}
                  <button type="button" class="btn" aria-label="Reconnect {a.label}" onclick={() => reconnect(a.id)}>Reconnect</button>
                {/if}
                <button type="button" class="btn" aria-label="Pause {a.label}" onclick={() => setPaused(a.id, true)}>Pause</button>
              {/if}
              <button type="button" class="btn" aria-label="Edit {a.label}" onclick={() => openEdit(a, 'name')}>Edit</button>
              <button type="button" class="btn text-trash" aria-label="Remove {a.label}" onclick={() => (removing = a.id)}>Remove</button>
            </div>
            {#if a.status === 'auth_failed'}
              <div class="flex flex-[1_1_100%] flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
                <span>Sign-in failed. Enter a new app password.</span>
                <button type="button" class="btn" aria-label="New app password for {a.label}" onclick={() => openEdit(a, 'password')}>New app password</button>
              </div>
            {/if}
            {#if a.status === 'cert_changed'}
              {@const c = certs[a.id]}
              {#if typeof c === 'object'}
                <div class="flex-[1_1_100%]">
                  <CertCheck cert={c.cert} message={c.message} busy={c.busy} onaccept={() => accept(a.id)} oncancel={() => delete certs[a.id]} />
                </div>
              {:else}
                <div class="flex flex-[1_1_100%] flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
                  <span>The server's certificate changed. Check the new one before MailRules connects again.</span>
                  <button type="button" class="btn" aria-label="Check certificate for {a.label}" disabled={c === 'checking'} onclick={() => reviewCert(a.id)}>{c === 'checking' ? 'Checking…' : 'Check certificate'}</button>
                </div>
              {/if}
            {/if}
          {:else if a.status === 'auth_failed'}
            <p class="flex-[1_1_100%] rounded bg-trash-bg px-3 py-2 text-trash">Sign-in failed. The person who added this mailbox needs to enter a new app password.</p>
          {:else if a.status === 'cert_changed'}
            <p class="flex-[1_1_100%] rounded bg-trash-bg px-3 py-2 text-trash">The server's certificate changed. The person who added this mailbox needs to check and accept the new one.</p>
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
