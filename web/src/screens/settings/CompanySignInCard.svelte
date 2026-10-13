<script lang="ts">
  import * as companySignIn from '../../lib/api/companySignIn';
  import { plan } from '../../lib/state/plan.svelte';
  import { settings } from '../../lib/state/settings.svelte';

  // Company sign-in (SAML or OIDC). While the team may use it (features.company_sign_in), its
  // Owners and Admins see how it stands and open the sign-in service's setup page. Otherwise the
  // shell shows this to an Owner only, in a build that sells it, to say how to get it.

  const wording: Record<companySignIn.Status, { label: string; chip: string; note: string }> = {
    not_set_up: { label: 'Not set up', chip: 'chip-neutral', note: 'Nobody signs in through your company yet.' },
    domain_pending: {
      label: 'Domain not verified',
      chip: 'chip-review',
      note: "The connection is set up and waits for your company's email domain to be verified. Until then nobody signs in through it.",
    },
    active: { label: 'Active', chip: 'chip-live', note: "Your team signs in through your company's identity provider." },
  };

  const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

  const allowed = $derived(settings.value.features.company_sign_in);
  let status = $state<companySignIn.Status | null>(null);
  let error = $state('');
  let opening = $state<companySignIn.Intent | ''>('');
  let portalError = $state('');

  async function load() {
    try {
      status = (await companySignIn.status()).status;
      error = '';
    } catch (e) {
      error = message(e);
    }
  }

  $effect(() => {
    if (allowed) load();
  });

  // The link works once and for minutes: it is asked for on the click and followed at once.
  async function open(intent: companySignIn.Intent) {
    portalError = '';
    opening = intent;
    try {
      window.location.assign((await companySignIn.portal(intent)).url);
    } catch (e) {
      portalError = message(e);
    } finally {
      opening = '';
    }
  }

  // How a team gets it, for an Owner: the Team plan's terms, priced at the plan's interval.
  const offer = $derived.by(() => {
    const info = plan.value;
    const cs = info?.company_sign_in;
    const terms = info?.plans.find((p) => p.id === 'team')?.company_sign_in;
    if (!info || !cs || !terms) return '';
    const price = terms.prices[info.interval ?? 'month'] ?? '';
    const n = cs.included_seats;
    if (info.plan !== 'team') {
      return n === null
        ? `It is part of the Team plan, as an add-on for ${price}. Choose Team on Plan and billing.`
        : `It is part of the Team plan: included from ${n} seats, or as an add-on for ${price} below that. Choose Team on Plan and billing.`;
    }
    if (n === null) return `Add it to your Team plan on Plan and billing, for ${price}.`;
    return `It comes with the Team plan from ${n} seats, and your plan has ${info.seats ?? 0}. Add it on Plan and billing for ${price}, or buy more seats.`;
  });
</script>

<section aria-label="Company sign-in" class="card flex flex-col gap-3.5 p-5">
  <h2>Company sign-in</h2>
  <p class="text-[13.5px] text-secondary">Your team signs in through your company's identity provider (SAML or OIDC) instead of a password or code.</p>

  {#if allowed}
    {#if error}
      <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2.5 text-[13px] text-trash">
        <span>Could not ask the sign-in service how it stands. {error}</span>
        <button type="button" class="btn" onclick={load}>Retry</button>
      </div>
    {:else if status}
      {@const w = wording[status]}
      <p class="flex flex-wrap items-center gap-2 text-[13px]">
        <span class="chip {w.chip}">{w.label}</span>
        <span class="text-secondary">{w.note}</span>
      </p>
      <div class="flex flex-wrap gap-2">
        <button type="button" class={status === 'not_set_up' ? 'btn-primary' : 'btn'} disabled={!!opening} onclick={() => open('sso')}>
          {opening === 'sso' ? 'Opening…' : status === 'not_set_up' ? 'Set up company sign-in' : 'Change the connection'}
        </button>
        <button type="button" class={status === 'domain_pending' ? 'btn-primary' : 'btn'} disabled={!!opening} onclick={() => open('domain_verification')}>
          {opening === 'domain_verification' ? 'Opening…' : 'Verify your domain'}
        </button>
      </div>
      <p class="text-[12.5px] text-secondary">Each button opens the sign-in service's setup page in this tab, with a link that works once, for a few minutes. Your IT admin can do it from here: invite them as an Admin first.</p>
      {#if portalError}<p role="alert" class="text-[13px] text-trash">{portalError}</p>{/if}
    {/if}
  {:else if offer}
    <p class="text-[13px]">{offer}</p>
    <a href="#/plan" class="btn self-start">Open Plan and billing</a>
  {/if}
</section>
