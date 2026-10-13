<script lang="ts">
  import * as membersApi from '../lib/api/members';
  import type { Invitation, Member, Role, Team } from '../lib/api/members';
  import ConfirmBox from '../lib/components/ConfirmBox.svelte';
  import Waiting from '../lib/components/Waiting.svelte';
  import { fullDay } from '../lib/format';
  import { auth, canManage, isOwner } from '../lib/state/auth.svelte';
  import { flash } from '../lib/state/toast.svelte';

  // Not in the prototype: drawn from the Settings and Mailboxes screens' cards, lists and forms.
  // Shown only while the plan includes the team (features.team). Everyone sees who is in it; an
  // Owner or Admin invites, changes roles and removes, and only an Owner touches an Owner.

  const roleNames: Record<Role, string> = { owner: 'Owner', admin: 'Admin', member: 'Member' };
  const asA: Record<Role, string> = { owner: 'an Owner', admin: 'an Admin', member: 'a Member' };
  // Derived: a person who changes their own role sees the screen as that role at once.
  const manage = $derived(canManage());
  const owner = $derived(isOwner());
  // The roles this person may hand out: Owner only by an Owner.
  const offered = $derived<Role[]>(owner ? ['owner', 'admin', 'member'] : ['admin', 'member']);

  const message = (e: unknown) => (e instanceof Error && e.message) || 'The daemon did not answer.';

  let team = $state<Team | null>(null);
  let invitations = $state<Invitation[]>([]);
  let loadError = $state('');
  // Each row's last refusal, by member or invitation id.
  let refused = $state<Record<string, string>>({});
  // The member whose removal is waiting to be confirmed.
  let asking = $state(0);

  let email = $state('');
  let inviteRole = $state<Role>('member');
  let inviting = $state(false);
  let inviteError = $state('');
  // On a plan sold by the seat (the daemon gives its price), an invitation while every seat is
  // taken buys one more at once; removing someone or revoking an invitation gives one back.
  const perSeat = $derived(team?.seats.price ?? null);
  const addsSeat = $derived(!!team && perSeat !== null && team.seats.limit !== null && team.seats.used >= team.seats.limit);

  async function load() {
    loadError = '';
    try {
      const [t, inv] = await Promise.all([membersApi.list(), manage ? membersApi.invitations() : null]);
      team = t;
      if (inv) invitations = inv.invitations;
    } catch (e) {
      loadError = message(e);
    }
  }
  load();

  /** Seats count members and invitations waiting, so both refresh them. */
  async function refreshSeats() {
    try {
      team = await membersApi.list();
    } catch {
      // The list already shown stays; the next visit reloads it.
    }
  }

  async function setRole(m: Member, select: HTMLSelectElement) {
    refused['m' + m.id] = '';
    try {
      const { member } = await membersApi.setRole(m.id, select.value as Role);
      Object.assign(m, member);
      if (member.you && auth.user) auth.user.role = member.role;
      flash(`${member.email} is now ${asA[member.role]}`);
    } catch (e) {
      select.value = m.role;
      refused['m' + m.id] = message(e);
    }
  }

  async function remove(m: Member) {
    asking = 0;
    refused['m' + m.id] = '';
    try {
      await membersApi.remove(m.id);
      flash(m.email + ' removed from the team');
      await refreshSeats();
    } catch (e) {
      refused['m' + m.id] = message(e);
    }
  }

  async function invite(e: SubmitEvent) {
    e.preventDefault();
    inviteError = '';
    inviting = true;
    try {
      const added = addsSeat;
      const { invitation } = await membersApi.invite(email.trim(), inviteRole);
      invitations.push(invitation);
      email = '';
      flash('Invitation sent to ' + invitation.email + (added ? '; a seat was added to your plan' : ''));
      await refreshSeats();
    } catch (err) {
      inviteError = message(err);
    } finally {
      inviting = false;
    }
  }

  async function revoke(i: Invitation) {
    refused['i' + i.id] = '';
    try {
      await membersApi.revoke(i.id);
      invitations = invitations.filter((x) => x.id !== i.id);
      flash('Invitation to ' + i.email + ' revoked');
      await refreshSeats();
    } catch (e) {
      refused['i' + i.id] = message(e);
    }
  }
</script>

<div class="flex min-w-0 flex-col gap-[18px]">
  <header>
    <h1>Members</h1>
    <p class="mt-1 text-secondary">Who is in your team. Everyone's mail is sorted by the team's shared rules.</p>
  </header>

  {#if loadError}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
      <span>{loadError}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {:else if !team}
    <Waiting text="Loading your team" />
  {:else}
    {@const seats = team.seats}
    <!-- A seat an invitation just bought counts before the billing service reports it. -->
    {@const limit = seats.limit !== null && seats.price !== null ? Math.max(seats.limit, seats.used) : seats.limit}
    <p class="text-[13px] text-secondary">
      {limit === null ? `${seats.used} ${seats.used === 1 ? 'seat' : 'seats'} used` : `${seats.used} of ${limit} seats used`}{manage ? ', invitations waiting included.' : '.'}
      {#if owner && seats.limit !== null}<a href="#/plan" class="underline">Change seats in Plan and billing</a>{/if}
    </p>

    <section aria-label="Team members" class="card overflow-hidden">
      {#each team.members as m, i (m.id)}
        <div class="flex flex-col gap-2.5 px-[18px] py-3.5 {i ? 'border-t border-line-divider' : ''}">
          <div class="flex flex-wrap items-center gap-x-4 gap-y-2">
            <div class="min-w-0 flex-[1_1_220px]">
              <div class="font-semibold break-words">
                {m.email}
                {#if m.you}<span class="chip chip-neutral ml-1.5">You</span>{/if}
              </div>
              <div class="text-[12.5px] text-muted">Joined {fullDay(m.joined_at)}</div>
            </div>
            {#if manage && (m.role !== 'owner' || owner)}
              <label class="sr-only" for="role-{m.id}">Role of {m.email}</label>
              <select id="role-{m.id}" class="field px-2.5" value={m.role} onchange={(e) => setRole(m, e.currentTarget)}>
                {#each offered.includes(m.role) ? offered : [m.role, ...offered] as r (r)}
                  <option value={r}>{roleNames[r]}</option>
                {/each}
              </select>
              {#if !m.you}
                <button type="button" class="btn" aria-label="Remove {m.email}" aria-expanded={asking === m.id} onclick={() => (asking = m.id)}>Remove</button>
              {/if}
            {:else}
              <span class="chip">{roleNames[m.role]}</span>
            {/if}
          </div>
          {#if asking === m.id}
            <ConfirmBox
              question="Remove {m.email} from the team?"
              note={'They are signed out at once. The mailboxes they connected stop now and are deleted after 30 days; the team keeps its rules.' + (perSeat ? " Your plan's seats then come down to those in use, credited on your next bill." : '')}
              confirm="Yes, remove them"
              onconfirm={() => remove(m)}
              oncancel={() => (asking = 0)}
            />
          {/if}
          {#if refused['m' + m.id]}<p role="alert" class="text-[13px] text-trash">{refused['m' + m.id]}</p>{/if}
        </div>
      {/each}
    </section>

    {#if manage}
      <section aria-label="Invitations" class="card flex flex-col gap-3.5 p-5">
        <h2 class="text-[15px]">Invitations</h2>
        {#each invitations as inv (inv.id)}
          <div class="flex flex-col gap-1.5 rounded border border-line-divider p-3">
            <div class="flex flex-wrap items-center gap-x-3 gap-y-2">
              <div class="flex min-w-0 flex-[1_1_220px] flex-col gap-0.5">
                <span class="font-semibold break-words">{inv.email}</span>
                <span class="text-[12.5px] text-secondary">
                  {roleNames[inv.role]}{inv.invited_by ? ` · invited by ${inv.invited_by}` : ''} · {inv.accepted_at ? 'accepted, joins at first sign-in' : `expires ${fullDay(inv.expires_at)}`}
                </span>
              </div>
              <button type="button" class="btn" aria-label="Revoke the invitation to {inv.email}" onclick={() => revoke(inv)}>Revoke</button>
            </div>
            {#if refused['i' + inv.id]}<p role="alert" class="text-[13px] text-trash">{refused['i' + inv.id]}</p>{/if}
          </div>
        {:else}
          <p class="text-[13px] text-secondary">No invitations waiting.</p>
        {/each}

        <form novalidate class="flex flex-col gap-3 border-t border-line-divider pt-3.5" onsubmit={invite}>
          <h3 class="font-semibold">Invite someone</h3>
          <div class="flex flex-wrap items-end gap-3">
            <div class="flex min-w-0 flex-[1_1_240px] flex-col gap-1.5">
              <label for="invite-email" class="text-[13px] font-semibold">Email</label>
              <input id="invite-email" type="email" class="field h-11" autocomplete="off" placeholder="name@example.com" bind:value={email} aria-invalid={inviteError ? true : undefined} />
            </div>
            <div class="flex flex-col gap-1.5">
              <label for="invite-role" class="text-[13px] font-semibold">Role</label>
              <select id="invite-role" class="field h-11 px-2.5" bind:value={inviteRole}>
                {#each offered as r (r)}<option value={r}>{roleNames[r]}</option>{/each}
              </select>
            </div>
            <button type="submit" class="btn-primary min-h-11 px-4" disabled={inviting || !email.trim()}>{inviting ? 'Sending…' : addsSeat ? 'Add a seat and send' : 'Send invitation'}</button>
          </div>
          <p class="text-[12.5px] text-secondary">
            {#if addsSeat}Every seat is taken, so this invitation adds one to your plan at once: {perSeat}, charged now for the rest of the billing period. Revoking it, or removing a member, brings your seats back down to those in use, credited on your next bill.{:else}The invitation takes a seat.{/if}
            It expires after 7 days. An Admin manages the shared rules, settings and members; a Member's own mailboxes are sorted by them.
          </p>
          {#if inviteError}<p role="alert" class="text-[13px] text-trash">{inviteError}</p>{/if}
        </form>
      </section>

      <section aria-label="Audit log" class="card flex flex-col gap-2.5 p-5">
        <h2 class="text-[15px]">Audit log</h2>
        <p class="text-[13px] text-secondary">Who changed the team's rules, sender rules, mailboxes, members, settings and plan, over the last 365 days. Names and addresses only, never email content.</p>
        <div class="flex flex-wrap gap-2">
          <a class="btn no-underline" href="/api/audit?format=csv" download>Download CSV</a>
          <a class="btn no-underline" href="/api/audit?format=json" download>Download JSON</a>
        </div>
      </section>
    {:else}
      <p class="text-[13px] text-secondary">An Owner or Admin of your team invites people and changes roles.</p>
    {/if}
  {/if}
</div>
