<script lang="ts">
  import { ApiError } from '../lib/api/client';
  import { auth, login, setup } from '../lib/state/auth.svelte';

  const first = $derived(auth.status === 'setup');
  let email = $state('');
  let password = $state('');
  let busy = $state(false);
  let error = $state<{ message: string; path?: string } | null>(null);

  async function submit(e: SubmitEvent) {
    e.preventDefault();
    busy = true;
    error = null;
    try {
      await (first ? setup : login)({ email, password });
    } catch (err) {
      if (!(err instanceof ApiError)) throw err;
      error = err;
    } finally {
      busy = false;
    }
  }
</script>

<main class="mx-auto flex min-h-screen max-w-[420px] flex-col justify-center gap-6 p-6">
  <div>
    <h1>{first ? 'Set up MailRules' : 'Sign in'}</h1>
    <p class="mt-2 text-secondary">
      {first
        ? 'Create the admin account for this install. It stays on this machine.'
        : 'Use the admin account you created at setup.'}
    </p>
  </div>
  <form class="flex flex-col gap-4" onsubmit={submit}>
    <label class="flex flex-col gap-1.5">
      <span class="label">Email</span>
      <input class="field h-11" type="email" autocomplete="username" required bind:value={email} aria-invalid={error?.path === 'email'} />
    </label>
    <label class="flex flex-col gap-1.5">
      <span class="label">Password{first ? ' (12 characters or more)' : ''}</span>
      <input
        class="field h-11"
        type="password"
        autocomplete={first ? 'new-password' : 'current-password'}
        required
        minlength={first ? 12 : undefined}
        bind:value={password}
        aria-invalid={error?.path === 'password'}
      />
    </label>
    {#if error}
      <p role="alert" class="rounded bg-trash-bg px-3 py-2 text-trash">{error.message}</p>
    {/if}
    <button class="btn-primary min-h-11" disabled={busy}>{first ? 'Create account' : 'Sign in'}</button>
  </form>
</main>
