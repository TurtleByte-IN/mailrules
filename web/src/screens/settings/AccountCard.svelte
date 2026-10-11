<script lang="ts">
  import { tick } from 'svelte';
  import { auth, deleteAccount } from '../../lib/state/auth.svelte';

  // Shown only when the build's sign-in module can delete people at its sign-in service
  // (features.delete_account): the hosted version. Self-host has no such card.

  let asking = $state(false);
  let typed = $state('');
  let deleting = $state(false);
  // Why the last try was refused (a wrong email, the sign-in service failing): nothing was deleted.
  let problem = $state('');
  let opener = $state<HTMLButtonElement>();
  let field = $state<HTMLInputElement>();

  const email = $derived(auth.user?.email ?? '');
  // The daemon compares the same way: without case and surrounding spaces.
  const matches = $derived(email !== '' && typed.trim().toLowerCase() === email.toLowerCase());
  const team = $derived(auth.members > 1);

  async function open() {
    asking = true;
    typed = '';
    problem = '';
    await tick();
    field?.focus();
  }

  function cancel() {
    asking = false;
    opener?.focus();
  }

  function onkeydown(e: KeyboardEvent) {
    if (e.key !== 'Escape' || deleting) return;
    e.stopPropagation();
    cancel();
  }

  async function confirm(e: SubmitEvent) {
    e.preventDefault();
    if (!matches || deleting) return;
    deleting = true;
    problem = '';
    try {
      // On success the session is gone and the app shows the signed-out page.
      await deleteAccount(typed.trim());
    } catch (err) {
      problem = (err as Error).message;
      deleting = false;
    }
  }
</script>

<section aria-label="Account" class="card flex flex-col gap-3.5 p-5">
  <h2>Account</h2>
  <p class="text-[13.5px] text-secondary">Signed in as {email}.</p>
  <button bind:this={opener} type="button" class="btn self-start" aria-expanded={asking} onclick={open}>Delete my account</button>

  {#if asking}
    <div
      role="alertdialog"
      aria-labelledby="del-q"
      aria-describedby="del-what"
      tabindex="-1"
      class="rounded border border-warn-line bg-warn-bg px-3 py-3 text-[13px] text-warn"
      {onkeydown}
    >
      <form novalidate class="flex flex-col gap-3" onsubmit={confirm}>
        <h3 id="del-q" class="font-semibold">Delete your account?</h3>
        <div id="del-what" class="flex flex-col gap-1.5">
          <p>
            <span class="font-semibold">Right away:</span> you are signed out, your mailboxes stop being sorted, and their passwords and sign-in tokens are deleted{team ? '.' : ', with your model API keys and notification addresses.'}
          </p>
          {#if team}
            <p><span class="font-semibold">Within 30 days:</span> your mailboxes and their activity are deleted. Your team keeps its rules and settings.</p>
          {:else}
            <p><span class="font-semibold">Within 30 days:</span> your rules, activity and settings are deleted.</p>
          {/if}
          <p>This cannot be undone.</p>
        </div>
        <div class="flex flex-col gap-1.5">
          <label for="del-email" class="font-semibold">Type your email address, {email}, to confirm</label>
          <input
            bind:this={field}
            bind:value={typed}
            id="del-email"
            type="email"
            class="field h-11 max-w-[360px]"
            autocomplete="off"
            spellcheck="false"
            disabled={deleting}
            aria-invalid={problem ? true : undefined}
            aria-describedby={problem ? 'del-problem' : undefined}
          />
        </div>
        {#if problem}
          <p id="del-problem" role="alert" class="text-trash">{problem}</p>
        {/if}
        <span class="flex flex-wrap gap-2">
          <button type="button" class="btn" disabled={deleting} onclick={cancel}>Cancel</button>
          <button type="submit" class="btn-primary bg-trash" disabled={!matches || deleting}>{deleting ? 'Deleting…' : 'Delete my account'}</button>
        </span>
      </form>
    </div>
  {/if}
</section>
