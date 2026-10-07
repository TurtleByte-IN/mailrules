<script lang="ts">
  import type { ActivityItem, FixRequest } from '../../lib/api/activity';

  // "Always do this": the sender rule a fix also stores, for the whole domain unless the address is chosen.
  // Used by the decision panel and the review card, which read value() for the request and hand the answer to settle().
  let { item }: { item: ActivityItem } = $props();

  const uid = $props.id();
  // An email without a sender domain can only have a rule for its address.
  const domain = $derived(item.from_domain);
  let ticked = $state(false);
  let scope = $state<NonNullable<FixRequest['always_for']>>('domain');
  let refusal = $state('');

  /** What the fix request carries; undefined while the box is not ticked. */
  export const value = () => (ticked ? (domain ? scope : 'address') : undefined);

  /** After a fix: a refusal of the whole domain is shown and the choice falls back to the address; a fix that went through unticks the box. */
  export function settle(said: string) {
    refusal = said;
    if (said) scope = 'address';
    else ticked = false;
  }
</script>

<div class="flex flex-col gap-1">
  <label id="{uid}-for" class="flex min-h-8 items-center gap-2 text-[13px] text-nav max-md:min-h-11">
    <input type="checkbox" bind:checked={ticked} />Always do this for {domain || item.from}
  </label>
  {#if ticked && domain}
    <div role="radiogroup" aria-labelledby="{uid}-for" class="flex flex-col pl-6 text-[13px] text-nav" onchange={() => (refusal = '')}>
      <label class="flex min-h-8 items-center gap-2 max-md:min-h-11"><input type="radio" name={uid} value="domain" bind:group={scope} />Everyone at {domain}</label>
      <label class="flex min-h-8 items-center gap-2 max-md:min-h-11"><input type="radio" name={uid} value="address" bind:group={scope} />Only {item.from}</label>
      {#if refusal}<p role="alert" class="rounded bg-trash-bg px-3 py-2 break-words text-trash">{refusal}</p>{/if}
    </div>
  {/if}
</div>
