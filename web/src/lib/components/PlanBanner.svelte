<script lang="ts">
  import { fullDay } from '../format';
  import { isOwner } from '../state/auth.svelte';
  import { plan } from '../state/plan.svelte';
  import { settings } from '../state/settings.svelte';

  // Only a build that sells plans has one; a plan left over from before the settings said so is not shown.
  const info = $derived(settings.value.features.billing ? plan.value : null);
  // Everyone is told; only an Owner can buy a plan, so everyone else is told who can.
  const owner = $derived(isOwner());
</script>

{#if info?.read_only}
  {@const gone = info.secrets_deleted_at !== null && info.secrets_deleted_at * 1000 <= Date.now()}
  <div role="status" class="flex flex-wrap items-center justify-between gap-2.5 rounded-md border border-warn-line bg-warn-bg px-4 py-3 text-warn">
    <span>
      <strong>{info.ended_at === info.trial_ends_at ? 'Your trial has ended.' : 'Your plan has ended.'}</strong>
      MailRules has stopped sorting. Activity, rules and settings stay readable.
      {#if info.secrets_deleted_at && info.removed_at}
        Mailbox passwords {gone ? 'were' : 'are'} deleted on {fullDay(info.secrets_deleted_at)} and everything else on {fullDay(info.removed_at)}.
      {/if}
    </span>
    {#if owner}
      <a href="#/plan" class="inline-flex min-h-9 items-center rounded border border-warn-strong bg-surface px-3 font-semibold text-warn max-md:min-h-11">Subscribe</a>
    {:else}
      <span class="font-semibold">Ask the team's owner to subscribe.</span>
    {/if}
  </div>
{:else if info?.plan === 'trial' && info.trial_days_left !== null && info.trial_days_left <= 3}
  <p class="text-[13px] text-secondary">
    Your trial ends in {info.trial_days_left} {info.trial_days_left === 1 ? 'day' : 'days'}.
    {#if owner}<a href="#/plan" class="font-semibold underline">Choose a plan</a>{:else}Ask the team's owner to subscribe.{/if}
  </p>
{/if}
