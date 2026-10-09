<script lang="ts">
  import { tick } from 'svelte';
  import type { Account, AccountPatch } from '../lib/api/accounts';
  import { ApiError } from '../lib/api/client';
  import { clock, day } from '../lib/format';
  import { accounts, edit, folderNames, load, loadPresets, reconnect, remove, setPaused, statuses, test } from '../lib/state/accounts.svelte';
  import { auth } from '../lib/state/auth.svelte';
  import { flash } from '../lib/state/toast.svelte';
  import Toggle from '../lib/components/Toggle.svelte';
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
  // The Edit form of the mailbox being edited. A new password lives here only, until it is sent or the form closes.
  let form = $state({ label: '', folder: '', password: '', shared: false, folders: [] as string[], error: '', errorPath: '', busy: false });

  // The list names each provider by its preset label.
  if (!accounts.presets.length) loadPresets();

  const provider = (a: Account) => accounts.presets.find((p) => p.name === a.preset)?.label ?? a.host;
  const detail = (a: Account) =>
    a.status === 'live'
      ? a.capabilities.includes('IDLE') ? 'push (IDLE) connected' : 'checked once a minute'
      : a.last_error || statuses[a.status].label.toLowerCase();

  async function runTest(id: number) {
    testing[id] = true;
    await test(id);
    testing[id] = false;
  }

  async function openEdit(a: Account, focus: 'name' | 'password') {
    form = { label: a.label, folder: a.watch_folder, password: '', shared: a.shared, folders: [a.watch_folder], error: '', errorPath: '', busy: false };
    editing = a.id;
    await tick();
    document.getElementById('edit-' + focus)?.focus();
    const names = await folderNames(a.id);
    if (editing === a.id && names.length) form.folders = names.includes(a.watch_folder) ? names : [a.watch_folder, ...names];
  }

  function closeEdit() {
    editing = undefined;
    form.password = '';
  }

  /** Sends only what changed; an empty password field means the stored one stays. */
  async function save(e: SubmitEvent, a: Account) {
    e.preventDefault();
    const p: AccountPatch = {};
    if (form.label.trim() !== a.label) p.label = form.label.trim();
    if (form.folder !== a.watch_folder) p.watch_folder = form.folder;
    if (form.password.trim()) p.password = form.password;
    if (form.shared !== a.shared) p.shared = form.shared;
    form.password = '';
    if (!Object.keys(p).length) return closeEdit();
    form.busy = true;
    form.errorPath = '';
    try {
      await edit(a.id, p);
      closeEdit();
    } catch (err) {
      if (err instanceof ApiError && err.path && err.path in p) [form.error, form.errorPath] = [err.message, err.path];
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
      <p class="mt-1 text-secondary">Any IMAP mailbox. MailRules sorts on the server, so every app you use sees the result.</p>
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
          {:else if a.status === 'auth_failed'}
            <p class="flex-[1_1_100%] rounded bg-trash-bg px-3 py-2 text-trash">Sign-in failed. The person who added this mailbox needs to enter a new app password.</p>
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
