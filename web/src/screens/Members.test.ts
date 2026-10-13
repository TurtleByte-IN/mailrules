import { fireEvent, render, screen, waitFor, within } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Invitation, Member, Role } from '../lib/api/members';
import { error, serve } from '../lib/state/activity.fixtures';
import { auth } from '../lib/state/auth.svelte';
import Members from './Members.svelte';

const member = (id: number, email: string, role: Role, you = false): Member => ({ id, email, role, joined_at: 1791000000, you });
const invitation = (id: number, email: string, role: Role = 'member'): Invitation => ({
  id, email, role, invited_by: 'ana@example.com', created_at: 1791000000, expires_at: 1791604800, accepted_at: null,
});

const people = [member(1, 'ana@example.com', 'owner'), member(2, 'ben@example.com', 'admin'), member(3, 'cy@example.com', 'member')];

/** A daemon with this team and these seats; `other` answers every call besides the two lists. */
function daemon(you: number, other: (call: string, body: unknown) => [number, unknown?] = () => [404], seats: { used: number; limit: number | null; price: string | null } = { used: 4, limit: 5, price: null }) {
  const team = { members: people.map((m) => ({ ...m, you: m.id === you })), seats };
  auth.user = { id: you, email: people[you - 1].email, role: people[you - 1].role };
  return serve((call, body) => {
    if (call === 'GET /api/members') return [200, team];
    if (call === 'GET /api/invitations') return [200, { invitations: [invitation(9, 'dee@example.com')] }];
    return other(call, body);
  });
}
const list = () => screen.findByRole('region', { name: 'Team members' });

afterEach(() => {
  vi.unstubAllGlobals();
  auth.user = null;
});

it('lists everyone with their role and joined date, marks you, and counts the seats', async () => {
  daemon(2);
  render(Members);
  const region = await list();
  expect(region.textContent).toContain('ana@example.com');
  expect(region.textContent).toContain('cy@example.com');
  expect(within(region).getByText('You').parentElement!.textContent).toContain('ben@example.com');
  expect(screen.getByText(/4 of 5 seats used/)).toBeTruthy();
});

describe('an Admin', () => {
  it('changes a member’s role but not an Owner’s, and is offered no Owner role', async () => {
    const calls = daemon(2, (call) => (call === 'PATCH /api/members/3' ? [200, { member: member(3, 'cy@example.com', 'admin') }] : [404]));
    render(Members);
    await list();
    expect(screen.queryByLabelText('Role of ana@example.com')).toBeNull();
    const select = screen.getByLabelText('Role of cy@example.com') as HTMLSelectElement;
    expect([...select.options].map((o) => o.value)).toEqual(['admin', 'member']);
    expect([...(screen.getByLabelText('Role') as HTMLSelectElement).options].map((o) => o.value)).toEqual(['admin', 'member']);

    await fireEvent.change(select, { target: { value: 'admin' } });
    await waitFor(() => expect(calls.find((c) => c.call === 'PATCH /api/members/3')?.body).toEqual({ role: 'admin' }));
  });

  it('removes someone after confirming, but never themselves', async () => {
    const calls = daemon(2, (call) => (call === 'DELETE /api/members/3' ? [204] : [404]));
    render(Members);
    await list();
    expect(screen.queryByRole('button', { name: 'Remove ben@example.com' })).toBeNull();
    await fireEvent.click(screen.getByRole('button', { name: 'Remove cy@example.com' }));
    expect(calls.some((c) => c.call === 'DELETE /api/members/3')).toBe(false);
    await fireEvent.click(screen.getByRole('button', { name: 'Yes, remove them' }));
    await waitFor(() => expect(calls.some((c) => c.call === 'DELETE /api/members/3')).toBe(true));
  });

  it('invites by email with a role, and shows the daemon’s refusal as it is', async () => {
    let reply: [number, unknown] = [409, error('seat_limit', 'Every seat of your plan is taken. Add seats in Plan and billing, or remove someone.')];
    const calls = daemon(2, (call) => (call === 'POST /api/invitations' ? reply : [404]));
    render(Members);
    await list();
    await fireEvent.input(screen.getByLabelText('Email'), { target: { value: ' eve@example.com ' } });
    await fireEvent.change(screen.getByLabelText('Role'), { target: { value: 'admin' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Send invitation' }));
    expect((await screen.findByRole('alert')).textContent).toBe('Every seat of your plan is taken. Add seats in Plan and billing, or remove someone.');
    expect(calls.find((c) => c.call === 'POST /api/invitations')?.body).toEqual({ email: 'eve@example.com', role: 'admin' });

    reply = [201, { invitation: invitation(10, 'eve@example.com', 'admin') }];
    await fireEvent.click(screen.getByRole('button', { name: 'Send invitation' }));
    const invites = screen.getByRole('region', { name: 'Invitations' });
    expect(await within(invites).findByText('eve@example.com')).toBeTruthy();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('revokes an invitation, and offers the audit log as CSV and JSON', async () => {
    const calls = daemon(2, (call) => (call === 'DELETE /api/invitations/9' ? [204] : [404]));
    render(Members);
    await fireEvent.click(await screen.findByRole('button', { name: 'Revoke the invitation to dee@example.com' }));
    await waitFor(() => expect(screen.queryByText('dee@example.com')).toBeNull());
    expect(calls.some((c) => c.call === 'DELETE /api/invitations/9')).toBe(true);
    expect(screen.getByRole('link', { name: 'Download CSV' }).getAttribute('href')).toBe('/api/audit?format=csv');
    expect(screen.getByRole('link', { name: 'Download JSON' }).getAttribute('href')).toBe('/api/audit?format=json');
  });

  it('says an invitation past the paid seats adds one, at its price, and that removing gives it back', async () => {
    const calls = daemon(2, (call) => (call === 'POST /api/invitations' ? [201, { invitation: invitation(10, 'eve@example.com') }] : [404]), { used: 5, limit: 5, price: '$8 a seat a month' });
    render(Members);
    await list();
    const form = screen.getByRole('button', { name: 'Add a seat and send' }).closest('form')!;
    expect(form.textContent).toContain('this invitation adds one to your plan at once: $8 a seat a month, charged now for the rest of the billing period');
    expect(form.textContent).toContain('credited on your next bill');
    await fireEvent.click(screen.getByRole('button', { name: 'Remove cy@example.com' }));
    expect(screen.getByRole('alertdialog').textContent).toContain("Your plan's seats then come down to those in use, credited on your next bill.");

    await fireEvent.input(screen.getByLabelText('Email'), { target: { value: 'eve@example.com' } });
    await fireEvent.click(screen.getByRole('button', { name: 'Add a seat and send' }));
    await waitFor(() => expect(calls.some((c) => c.call === 'POST /api/invitations')).toBe(true));
  });

  it('says nothing of adding a seat while one is free', async () => {
    daemon(2, undefined, { used: 4, limit: 5, price: '$8 a seat a month' });
    render(Members);
    await list();
    expect(screen.getByRole('button', { name: 'Send invitation' }).closest('form')!.textContent).toContain('The invitation takes a seat.');
  });
});

it('lets an Owner change an Owner and hand out the Owner role', async () => {
  daemon(1);
  render(Members);
  await list();
  expect(screen.getByLabelText('Role of ana@example.com')).toBeTruthy();
  expect([...(screen.getByLabelText('Role of cy@example.com') as HTMLSelectElement).options].map((o) => o.value)).toEqual(['owner', 'admin', 'member']);
});

it('shows a refused role change and puts the role back', async () => {
  daemon(1, () => [409, error('last_owner', 'Your team needs at least one Owner. Make someone else an Owner first.')]);
  render(Members);
  await list();
  const select = screen.getByLabelText('Role of ana@example.com') as HTMLSelectElement;
  await fireEvent.change(select, { target: { value: 'admin' } });
  expect((await screen.findByRole('alert')).textContent).toBe('Your team needs at least one Owner. Make someone else an Owner first.');
  expect(select.value).toBe('owner');
});

it('shows a Member the team read-only, and asks for no invitations', async () => {
  const calls = daemon(3);
  render(Members);
  await list();
  expect(screen.queryByRole('combobox')).toBeNull();
  expect(screen.queryByRole('button', { name: /^Remove/ })).toBeNull();
  expect(screen.queryByRole('region', { name: 'Invitations' })).toBeNull();
  expect(screen.queryByRole('link', { name: 'Download CSV' })).toBeNull();
  expect(screen.getByText('An Owner or Admin of your team invites people and changes roles.')).toBeTruthy();
  expect(calls.some((c) => c.call === 'GET /api/invitations')).toBe(false);
});
