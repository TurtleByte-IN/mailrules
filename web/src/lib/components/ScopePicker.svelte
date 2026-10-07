<script lang="ts">
  import type { Folder } from '../api/cleanup';
  import { accounts } from '../state/accounts.svelte';
  import { settings } from '../state/settings.svelte';
  import { archiveFolder, scopeProblem, type Scope } from '../scope';

  /**
   * Mailbox, Folder and Which emails, with the number box and what is wrong with it. The screen owns
   * the scope and applies `onchange`'s patch. `busy` locks every control; `locked` locks all but the
   * mailbox. `id` prefixes the problem line's id. Laid out as children of the screen's card.
   */
  let {
    scope,
    folders,
    busy = false,
    locked = false,
    id,
    onchange,
  }: { scope: Scope; folders: Folder[]; busy?: boolean; locked?: boolean; id: string; onchange: (patch: Partial<Scope>) => void } = $props();

  const choices: Record<Scope['mode'], string> = { newest: 'Newest emails', days: 'From the last days', all: 'All mail' };
  const problem = $derived(scopeProblem(scope));
  // The number of the chosen entry, for the box; all mail has none.
  const boxed = $derived(scope.mode === 'all' ? null : scope.mode);
  const typed = (e: Event & { currentTarget: HTMLInputElement }) => (Number.isNaN(e.currentTarget.valueAsNumber) ? null : e.currentTarget.valueAsNumber);
  const archive = $derived(archiveFolder(folders));
</script>

<div class="flex flex-wrap gap-3">
  <label class="flex flex-[1_1_200px] flex-col gap-1.5">
    <span class="text-[13px] font-semibold">Mailbox</span>
    <select class="field h-11 px-2.5" disabled={busy} value={scope.accountId} onchange={(e) => onchange({ accountId: e.currentTarget.value })}>
      {#each accounts.list as a (a.id)}
        <option value={String(a.id)}>{a.label}</option>
      {/each}
    </select>
  </label>
  <label class="flex flex-[1_1_160px] flex-col gap-1.5">
    <span class="text-[13px] font-semibold">Folder</span>
    <select class="field h-11 px-2.5" disabled={busy || locked} value={scope.folder} onchange={(e) => onchange({ folder: e.currentTarget.value })}>
      <option value="INBOX">Inbox</option>
      {#if archive}
        <option value={archive}>Archive</option>
      {/if}
    </select>
  </label>
  <label class="flex flex-[1_1_160px] flex-col gap-1.5">
    <span class="text-[13px] font-semibold">Which emails</span>
    <select class="field h-11 px-2.5" disabled={busy || locked} value={scope.mode} onchange={(e) => onchange({ mode: e.currentTarget.value as Scope['mode'] })}>
      {#each Object.entries(choices) as [value, name] (value)}
        <option {value}>{name}</option>
      {/each}
    </select>
  </label>
  {#if boxed}
    <div class="flex flex-[0_1_140px] flex-col gap-1.5">
      <span class="text-[13px] font-semibold">{boxed === 'newest' ? 'Emails' : 'Days'}</span>
      <input
        type="number"
        min="1"
        max={boxed === 'newest' ? settings.value.limits.check_max || undefined : undefined}
        step="1"
        inputmode="numeric"
        aria-label={boxed === 'newest' ? 'How many emails' : 'How many days'}
        aria-invalid={problem ? true : undefined}
        aria-describedby={problem ? `${id}-scope-problem` : undefined}
        class="field h-11 px-2.5 font-mono"
        disabled={busy || locked}
        value={scope[boxed]}
        oninput={(e) => onchange({ [boxed]: typed(e) })}
      />
    </div>
  {/if}
</div>

{#if problem}
  <p id="{id}-scope-problem" role="alert" class="-mt-2 text-[12.5px] text-trash">{problem}</p>
{/if}
