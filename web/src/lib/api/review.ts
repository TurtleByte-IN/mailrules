// DEMO: GET /api/review and POST /api/review/{message_id}/resolve are not in
// api/openapi.yaml yet (backend M7).
import { findRule, messages, redecide, row, type ActivityRow, type Message } from './activity';
import { fake } from './demo';

export interface ReviewItem {
  /** Message id. */
  id: string;
  /** Unix seconds. */
  receivedAt: number;
  sender: string;
  domain: string;
  subject: string;
  /** Plain text. Never HTML. */
  snippet: string;
  /** Rule id of the best guess. */
  suggest: string;
  confidence: number;
  reason: string;
  /** A rule the user could add so mail like this stops landing here, in their own kind of wording. */
  idea: string;
}

const ago = (minutes: number) => Math.floor(Date.now() / 1000) - minutes * 60;

let queue: ReviewItem[] = [
  { id: 'a8', receivedAt: ago(61), sender: 'JobAlerts', domain: 'jobalerts.in', subject: '12 new jobs matching Backend Developer', snippet: '12 new jobs this week matching Backend Developer in Bengaluru and remote.', suggest: 'r3', confidence: 0.58, reason: 'Job alert digest: Recruiters or Newsletters', idea: 'Job alert digests from job sites go to Jobs and get marked read' },
  { id: 'v2', receivedAt: ago(93), sender: 'Rohan, Acme Cloud', domain: 'acmecloud.io', subject: '15 minutes to cut your AWS bill?', snippet: 'Hi, teams like yours save 30% on cloud spend with Acme. Worth a quick call this week?', suggest: 'r5', confidence: 0.49, reason: 'Sales pitch; you have no rule for cold sales yet', idea: "Cold sales pitches from people I've never emailed go to Trash" },
  { id: 'v3', receivedAt: ago(107), sender: 'IRCTC', domain: 'irctc.co.in', subject: 'Booking confirmed: PNR 4521 Bengaluru to Mysuru', snippet: 'Your e-ticket is confirmed. Train 12614, Coach C3, departs 07:00 on Saturday.', suggest: 'r6', confidence: 0.55, reason: 'Travel booking; closest rule is Orders', idea: 'Train, flight and hotel bookings go to Travel' },
  { id: 'v4', receivedAt: ago(135), sender: 'Bengaluru Run Club', domain: 'blrrunclub.org', subject: 'Your bib number and race-day guide', snippet: 'Bib 2214. Reporting time 5:15 AM at Kanteerava Stadium. Bag drop closes at 5:45.', suggest: 'r5', confidence: 0.61, reason: 'Event logistics that look like a newsletter', idea: 'Race and event tickets stay in Inbox and get flagged' },
];

export const list = () => fake(queue);

/** ruleId null keeps the message in the Inbox. Resolves to its activity row, as decided by the user. */
export async function resolve(id: string, ruleId: string | null, always: boolean): Promise<ActivityRow> {
  const it = queue.find((x) => x.id === id)!;
  const r = await findRule(ruleId);
  let m = messages.find((x) => x.id === id);
  if (!m) {
    // The demo feed is one page long, so older review mail has no row in it yet.
    m = {
      id, receivedAt: ago(0), sender: it.sender, from: `${it.sender} <hello@${it.domain}>`, domain: it.domain, accountId: 'acc1',
      subject: it.subject, reason: it.reason, ruleId: null, rule: 'Needs review', kind: 'review', stage: 'fallback',
      confidence: it.confidence, outcome: 'In Inbox', actionId: null, undone: false, snippet: it.snippet,
      trace: [{ label: 'Decision model', detail: 'Unsure: best guess ' + it.confidence.toFixed(2), meta: '', active: false }],
    } satisfies Message;
    messages.unshift(m);
  }
  redecide(m, r, 'reviewed', (r ? 'You chose ' + r.name : 'You kept it in Inbox') + (always ? '; saved as a sender rule' : ''), {
    label: 'Your review',
    detail: r ? 'Chose ' + r.name : 'Kept in Inbox',
    meta: always ? 'Saved as a sender rule' : 'Saved as an example',
  });
  queue = queue.filter((x) => x.id !== id);
  return fake(row(m));
}
