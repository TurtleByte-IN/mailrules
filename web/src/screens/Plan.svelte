<script lang="ts">
  import type { Interval, PlanInfo, PlanTerms } from '../lib/api/plan';
  import * as planApi from '../lib/api/plan';
  import { openCheckout } from '../lib/checkout';
  import PlanMeters from '../lib/components/PlanMeters.svelte';
  import { fullDay } from '../lib/format';
  import { auth } from '../lib/state/auth.svelte';
  import { limits, load as loadLimits } from '../lib/state/limits.svelte';
  import { load, plan } from '../lib/state/plan.svelte';

  load();
  loadLimits();

  const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

  // The interval the cards price; the plan's own once it has one.
  let chosen = $state<Interval | null>(null);
  const interval = $derived<Interval>(chosen ?? plan.value?.interval ?? 'month');
  // The card a request is out for, and each card's refusal (409 downgrade_refused names the mailboxes to remove).
  let busy = $state('');
  let refused = $state<Record<string, string>>({});
  let portalError = $state('');
  let opening = $state(false);

  const nameOf = (info: PlanInfo) => info.plans.find((p) => p.id === info.plan)?.name ?? info.plan;
  const days = (d: number) => d + (d === 1 ? ' day' : ' days');

  function subtitle(info: PlanInfo) {
    if (info.plan === 'trial') return "You're on the free trial.";
    if (info.plan === 'ended') return info.ended_at === info.trial_ends_at ? 'Your trial has ended.' : 'Your plan has ended.';
    return `You're on ${nameOf(info)}, billed ${info.interval === 'year' ? 'yearly' : 'monthly'}.`;
  }

  // Where the plan stands: the trial's days, the next renewal, a cancelled plan's end, or a failed payment.
  function standing(info: PlanInfo) {
    if (info.plan === 'trial') return `${days(info.trial_days_left ?? 0)} left in your trial; it ends ${fullDay(info.trial_ends_at)}.`;
    if (info.plan === 'ended') return info.ended_at ? `Ended ${fullDay(info.ended_at)}. Choose a plan to sort again.` : '';
    if (info.status === 'past_due') return "Payment failed; the billing service is retrying it, and your plan keeps running. Check the payment method in Manage billing.";
    if (!info.period_end) return '';
    if (info.status === 'cancels_at_period_end') return `Ends ${fullDay(info.period_end)}; it won't renew.`;
    return `Renews ${fullDay(info.period_end)}.`;
  }

  async function pick(info: PlanInfo, p: PlanTerms) {
    busy = p.id;
    refused[p.id] = '';
    // A plan sold by the seat is bought with a seat count; any other must not send one.
    const choice = { plan: p.id, interval, ...(p.per_seat ? { seats: seatsOf(info, p) } : {}) };
    try {
      if (info.subscribed) {
        plan.value = await planApi.change(choice);
        plan.waiting = true;
      } else {
        const c = await planApi.checkout(choice);
        if ((await openCheckout(c.script, c.checkout)) === 'completed') plan.waiting = true;
      }
    } catch (e) {
      refused[p.id] = message(e);
    } finally {
      busy = '';
    }
  }

  // The seats each per-seat card asks for: the person's number once typed; else the seats bought
  // of that plan; else enough for everyone in the team, and never fewer than the plan's least.
  let seatsChosen = $state<Record<string, number>>({});
  const seatsOf = (info: PlanInfo, p: PlanTerms) =>
    seatsChosen[p.id] ?? (info.plan === p.id && info.seats ? info.seats : Math.max(p.min_seats ?? 1, auth.members));

  async function manage() {
    portalError = '';
    opening = true;
    try {
      window.location.assign((await planApi.portal()).url);
    } catch (e) {
      portalError = message(e);
    } finally {
      opening = false;
    }
  }
</script>

<div class="flex min-w-0 flex-col gap-[18px]">
  <header>
    <h1>Plan and billing<span class="chip chip-neutral ml-1.5 align-middle">Cloud only</span></h1>
    {#if plan.value}
      <p class="mt-1 text-secondary">{subtitle(plan.value)} Self-hosting MailRules is always free.</p>
    {/if}
  </header>

  {#if plan.error && !plan.value}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
      <span>{plan.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {:else if plan.value}
    {@const info = plan.value}
    {@const line = standing(info)}
    <section aria-label="Your plan" class="card flex flex-col gap-4 p-5">
      {#if limits.value && (limits.value.mailboxes || limits.value.ai_emails)}
        <PlanMeters limits={limits.value} />
      {/if}
      <p class="text-[13px] text-secondary">
        Your own model keys sort email without counting against the plan: <a href="#/settings" class="underline">add them in Settings</a>.
      </p>
      {#if line || info.note}
        <p class="text-[13px] {info.status === 'past_due' ? 'rounded border border-warn-line bg-warn-bg px-3 py-2.5 text-warn' : 'text-secondary'}">
          {[line, info.note].filter(Boolean).join(' ')}
        </p>
      {/if}
      {#if info.seats !== null}
        <p class="text-[13px] text-secondary">Your plan has {info.seats} {info.seats === 1 ? 'seat' : 'seats'}; your team has {auth.members} {auth.members === 1 ? 'member' : 'members'}.</p>
      {/if}
      {#if info.subscribed}
        <div class="flex flex-wrap items-center gap-3">
          <button type="button" class="btn self-start font-semibold" disabled={opening} onclick={manage}>{opening ? 'Opening…' : 'Manage billing'}</button>
          <span class="text-[12.5px] text-muted">Cancel, change the payment method, or get invoices.</span>
        </div>
        {#if portalError}<p role="alert" class="text-[13px] text-trash">{portalError}</p>{/if}
      {/if}
    </section>

    {#if plan.waiting}
      <p role="status" class="rounded border border-line-card bg-selected-row px-3 py-2.5 text-[13px]">Waiting for the billing service to confirm…</p>
    {/if}

    <div role="group" aria-label="Billing interval" class="flex gap-1 self-start rounded border border-line-card bg-surface p-1">
      {#each [['month', 'Monthly'], ['year', 'Yearly']] as const as [id, label] (id)}
        <button type="button" aria-pressed={interval === id} class="min-h-9 rounded-sm border-0 px-3.5 font-semibold {interval === id ? 'bg-ink text-surface' : 'bg-transparent text-secondary'}" onclick={() => (chosen = id)}>{label}</button>
      {/each}
    </div>

    <div class="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-3">
      {#each info.plans as p (p.id)}
        {@const samePlan = info.plan === p.id && info.interval === interval}
        {@const seats = p.per_seat ? seatsOf(info, p) : null}
        {@const current = samePlan && (seats === null || seats === info.seats)}
        <article aria-label={p.name} class="flex flex-col gap-2.5 rounded-md bg-surface p-[18px] {samePlan ? 'border-2 border-ink' : 'border border-line-card'}">
          <h2 class="text-[15px]">{p.name}</h2>
          <div class="text-[22px] font-semibold">{p.prices[interval] ?? ''}</div>
          {#if p.per_seat}
            <div class="-mt-2 text-[12.5px] text-muted">Per seat{p.min_seats ? `, from ${p.min_seats} seats` : ''}</div>
          {/if}
          <ul class="flex flex-1 list-disc flex-col gap-1 pl-[18px] text-[13px] text-nav">
            {#each p.includes as line (line)}<li>{line}</li>{/each}
          </ul>
          {#if p.per_seat}
            <div class="flex items-center gap-2">
              <label for="seats-{p.id}" class="text-[13px] font-semibold">Seats</label>
              <input
                id="seats-{p.id}"
                type="number"
                inputmode="numeric"
                class="field w-24"
                min={p.min_seats ?? 1}
                max="1000"
                value={seats}
                oninput={(e) => (seatsChosen[p.id] = e.currentTarget.valueAsNumber)}
              />
            </div>
          {/if}
          {#if current}
            <button type="button" class="min-h-10 rounded border border-line-card bg-neutral px-3.5 font-semibold text-muted" disabled>Current plan</button>
          {:else}
            <button type="button" class="min-h-10 rounded border border-ink bg-surface px-3.5 font-semibold text-ink" disabled={!!busy || !p.prices[interval] || (seats !== null && !Number.isInteger(seats))} onclick={() => pick(info, p)}>
              {busy === p.id ? 'Asking…' : samePlan ? 'Change seats' : (info.subscribed ? 'Switch to ' : 'Choose ') + p.name}
            </button>
          {/if}
          {#if refused[p.id]}<p role="alert" class="text-[13px] text-trash">{refused[p.id]}</p>{/if}
        </article>
      {/each}
    </div>
  {/if}
</div>
