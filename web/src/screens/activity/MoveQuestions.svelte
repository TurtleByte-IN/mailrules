<script lang="ts">
  import type { MoveQuestion } from '../../lib/api/questions';
  import { canManage } from '../../lib/state/auth.svelte';
  import { answer, questions } from '../../lib/state/questions.svelte';

  // The questions about emails the person moved in another mail app, asked once per sender
  // and destination (MAI-163). Yes stores the sender rule "Always do this for <sender>"
  // stores; No closes the question for good. Activity shows this only with the module that
  // learns from moves (features.move_questions).
  const uid = $props.id();
  let busy = $state<number | null>(null);

  async function reply(q: MoveQuestion, a: 'yes' | 'no') {
    busy = q.id;
    await answer(q, a);
    busy = null;
  }
</script>

{#if questions.list.length || questions.error}
  <section aria-label="Questions about emails you moved" class="flex flex-col gap-2">
    {#if questions.error}<p role="alert" class="text-[13px] text-trash">{questions.error}</p>{/if}
    {#each questions.list as q (q.id)}
      {@const where = q.inbox ? 'Inbox' : q.folder}
      <div role="group" aria-labelledby="{uid}-{q.id}" class="card flex flex-wrap items-center justify-between gap-x-4 gap-y-2 px-[18px] py-3.5">
        <div class="flex min-w-0 flex-[1_1_260px] flex-col gap-0.5">
          <span id="{uid}-{q.id}" class="font-semibold">
            You moved {q.emails} {q.emails === 1 ? 'email' : 'emails'} from this sender to {where}.
            {q.inbox ? 'Always keep them in Inbox?' : `Always move them to ${where}?`}
          </span>
          <span class="truncate text-xs text-muted">{q.from} · on {q.mailbox}</span>
          {#if !canManage()}
            <!-- Yes stores a sender rule for the team, which only an Owner or Admin may. -->
            <span class="text-xs text-secondary">Only an Owner or Admin can say Yes: it changes the team's sender rules.</span>
          {/if}
        </div>
        <span class="flex gap-2">
          <button type="button" class="btn" disabled={busy === q.id} onclick={() => reply(q, 'no')}>No</button>
          {#if canManage()}
            <button type="button" class="btn-primary" disabled={busy === q.id} onclick={() => reply(q, 'yes')}>Yes</button>
          {/if}
        </span>
      </div>
    {/each}
  </section>
{/if}
