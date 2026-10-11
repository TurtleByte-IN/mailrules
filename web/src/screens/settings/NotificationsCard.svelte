<script lang="ts">
  import { onMount } from 'svelte';
  import { ApiError } from '../../lib/api/client';
  import type { Kind } from '../../lib/api/notifications';
  import ConfirmBox from '../../lib/components/ConfirmBox.svelte';
  import { clock, weekday } from '../../lib/format';
  import { create, load, notifications, remove } from '../../lib/state/notifications.svelte';
  import { flash } from '../../lib/state/toast.svelte';

  // Shown only when the build has a module that delivers notifications (features.notifications).

  // Wording of each kind, from the prototype's channel list.
  const wording: Record<Kind, { label: string; ph: string; hint: string }> = {
    webhook: { label: 'Webhook', ph: 'https://example.com/mailrules', hint: 'MailRules POSTs JSON to it for every email a rule that names it acts on, signed with a secret shown once.' },
    slack: { label: 'Slack', ph: 'https://hooks.slack.com/services/…', hint: 'The incoming webhook URL of a Slack channel.' },
  };

  let name = $state('');
  let url = $state('');
  // The first kind this build offers until another is picked.
  let kind = $derived<Kind>(notifications.kinds[0] ?? 'webhook');
  let adding = $state(false);
  // A field the daemon refused in the last add ("name", "kind" or "url"), and why.
  let refused = $state<{ path: string; message: string } | null>(null);
  // A new webhook's signing secret: the daemon sends it this once.
  let secret = $state<{ name: string; value: string } | null>(null);
  let copied = $state(false);
  // The destination whose removal is waiting to be confirmed.
  let asking = $state(0);

  onMount(load);

  const bad = (field: string) => (refused?.path === field ? { 'aria-invalid': true, 'aria-describedby': `notif-${field}-problem` } : {});

  async function add(e: SubmitEvent) {
    e.preventDefault();
    refused = null;
    adding = true;
    try {
      const r = await create({ name: name.trim(), kind, url: url.trim() });
      name = '';
      url = '';
      copied = false;
      secret = r.secret ? { name: r.destination.name, value: r.secret } : null;
      if (!r.secret) flash(r.destination.name + ' added');
    } catch (err) {
      if (err instanceof ApiError && err.path && ['name', 'kind', 'url'].includes(err.path)) refused = { path: err.path, message: err.message };
      else flash((err as Error).message);
    } finally {
      adding = false;
    }
  }

  async function copy() {
    if (!secret) return;
    try {
      await navigator.clipboard.writeText(secret.value);
      copied = true;
    } catch {
      flash('Could not copy it. Select the secret and copy it yourself.');
    }
  }

  async function drop(id: number, label: string) {
    asking = 0;
    try {
      await remove(id);
      flash(label + ' removed');
    } catch (e) {
      flash((e as Error).message);
    }
  }
</script>

{#snippet problem(field: string)}
  {#if refused?.path === field}
    <p id="notif-{field}-problem" role="alert" class="text-[12.5px] text-trash">{refused.message}</p>
  {/if}
{/snippet}

<section aria-label="Notifications and webhooks" class="card flex flex-col gap-3.5 p-5">
  <h2>Notifications and webhooks</h2>
  <p class="text-[13.5px] text-secondary">Where rule notifications go. Each rule chooses which of these to tell, under More options. A notice says which rule acted on which email; it never carries the email's text.</p>

  {#if notifications.error}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">
      <span>Could not load where notifications go. {notifications.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {/if}

  {#if secret}
    <div role="status" class="flex flex-col gap-2 rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-[13px] text-warn">
      <span><span class="font-semibold">Signing secret for {secret.name}.</span> It is shown only now: copy it to the server that receives the webhook. Every delivery is signed with it.</span>
      <code class="rounded bg-surface px-2 py-1.5 font-mono text-[12.5px] break-all text-ink select-all">{secret.value}</code>
      <span class="flex flex-wrap gap-2">
        <button type="button" class="btn" onclick={copy}>{copied ? 'Copied' : 'Copy'}</button>
        <button type="button" class="btn-primary" onclick={() => (secret = null)}>Done</button>
      </span>
    </div>
  {/if}

  {#if notifications.loaded}
    {#each notifications.destinations as d (d.id)}
      <article aria-label={d.name} class="flex flex-col gap-2.5 rounded border border-line-divider p-3">
        <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
          <div class="flex min-w-0 flex-[1_1_220px] flex-col gap-0.5">
            <span class="font-semibold">{d.name}</span>
            <span class="text-[12.5px] text-secondary">{wording[d.kind]?.label ?? d.kind} · <span class="font-mono break-all">{d.url}</span></span>
          </div>
          <button type="button" class="btn" aria-label="Remove {d.name}" aria-expanded={asking === d.id} onclick={() => (asking = d.id)}>Remove</button>
        </div>
        {#if d.address_deleted}
          <p class="text-[13px] text-review">Its address was deleted with the sign-in of the team's only member, so nothing is delivered to it. Remove it and add it again.</p>
        {/if}
        {#if asking === d.id}
          <ConfirmBox question="Remove {d.name}?" note="Rules that notify it stop doing so, and its delivery log goes with it." confirm="Yes, remove it" onconfirm={() => drop(d.id, d.name)} oncancel={() => (asking = 0)} />
        {/if}
        <div class="flex flex-col gap-1">
          <h3 class="text-[13px] font-semibold">Recent deliveries</h3>
          {#if d.deliveries.length}
            <ul class="m-0 flex list-none flex-col gap-1 p-0 text-[12.5px]">
              {#each d.deliveries as x}
                <li class="flex flex-wrap gap-x-2">
                  <span class="text-muted">{weekday(x.at)}, {clock(x.at)}</span>
                  <span>{x.rule_name}</span>
                  {#if x.status === 'sent'}
                    <span class="text-secondary">Sent</span>
                  {:else}
                    <span class="text-trash">Failed: {x.reason}</span>
                  {/if}
                </li>
              {/each}
            </ul>
          {:else}
            <p class="text-[12.5px] text-secondary">Nothing delivered yet.</p>
          {/if}
        </div>
      </article>
    {:else}
      <p class="text-[13px] text-secondary">No destinations yet.</p>
    {/each}

    <form novalidate class="flex flex-col gap-3 border-t border-line-divider pt-3.5" onsubmit={add}>
      <h3 class="font-semibold">Add a destination</h3>
      <div class="flex flex-wrap gap-3">
        <div class="flex min-w-0 flex-[1_1_180px] flex-col gap-1.5">
          <label for="notif-name" class="text-[13px] font-semibold">Name</label>
          <input id="notif-name" class="field h-11" autocomplete="off" maxlength="100" placeholder="For example: Team channel" bind:value={name} {...bad('name')} />
          {@render problem('name')}
        </div>
        <div class="flex flex-col gap-1.5">
          <label for="notif-kind" class="text-[13px] font-semibold">Kind</label>
          <select id="notif-kind" class="field h-11 px-2.5" bind:value={kind} {...bad('kind')}>
            {#each notifications.kinds as k (k)}
              <option value={k}>{wording[k]?.label ?? k}</option>
            {/each}
          </select>
          {@render problem('kind')}
        </div>
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="notif-url" class="text-[13px] font-semibold">Address</label>
        <input id="notif-url" type="url" inputmode="url" class="field h-11 font-mono text-[13px]" autocomplete="off" placeholder={wording[kind]?.ph} bind:value={url} {...bad('url')} />
        <div class="text-[12.5px] text-secondary">{wording[kind]?.hint}</div>
        {@render problem('url')}
      </div>
      <button type="submit" class="btn-primary min-h-11 self-start px-4" disabled={adding}>{adding ? 'Adding…' : 'Add'}</button>
    </form>
  {/if}
</section>
