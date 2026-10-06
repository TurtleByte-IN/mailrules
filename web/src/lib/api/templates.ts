// DEMO: GET /api/templates is not in api/openapi.yaml yet (backend M9).
import { fake } from './demo';
import type { Action, ConditionTree } from './rules';

export interface Template {
  id: string;
  name: string;
  desc: string;
  intent: string | null;
  conditions: ConditionTree;
  actions: Action[];
}

const templates: Template[] = [
  { id: 'g1', name: 'Newsletters', desc: 'Newsletters and promos you rarely open', intent: 'Newsletters and promotional emails', conditions: {}, actions: [{ type: 'move', folder: 'Reading' }, { type: 'read' }] },
  { id: 'g2', name: 'Receipts', desc: 'Purchase receipts and invoices', intent: 'Receipts and invoices for purchases', conditions: {}, actions: [{ type: 'move', folder: 'Receipts' }] },
  { id: 'g3', name: 'Login codes', desc: 'OTPs and verification codes stay on top', intent: null, conditions: { all: [{ field: 'subject', op: 'contains_any', value: ['code', 'OTP', 'verification'] }] }, actions: [{ type: 'keep' }, { type: 'flag' }] },
  { id: 'g4', name: 'Cold sales', desc: 'Pitches from people you never emailed', intent: 'Cold sales pitches and unsolicited demo requests', conditions: {}, actions: [{ type: 'trash' }] },
  { id: 'g5', name: 'Travel', desc: 'Tickets, bookings and itineraries', intent: 'Train, flight and hotel bookings', conditions: {}, actions: [{ type: 'move', folder: 'Travel' }] },
  { id: 'g6', name: 'Social notifications', desc: 'Likes, follows and profile views', intent: null, conditions: { all: [{ field: 'from_domain', op: 'in', value: ['linkedin.com', 'facebookmail.com', 'x.com'] }] }, actions: [{ type: 'archive' }, { type: 'read' }] },
  { id: 'g7', name: 'Bank statements', desc: 'Statements and card bills to Finance', intent: 'Bank statements and credit card bills', conditions: { all: [{ field: 'has_attachment', op: 'eq', value: true }] }, actions: [{ type: 'move', folder: 'Finance' }] },
  { id: 'g8', name: 'Calendar invites', desc: 'Meeting invites stay in Inbox, flagged', intent: null, conditions: { all: [{ field: 'attachment_ext', op: 'in', value: ['ics'] }] }, actions: [{ type: 'keep' }, { type: 'flag' }] },
];

export const list = () => fake(templates);
