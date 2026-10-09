// Timestamps arrive as unix seconds and are shown in the browser's locale and zone.
const date = (ts: number) => new Date(ts * 1000);

/** 08:41 */
export const clock = (ts: number) => date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

/** 6 Oct */
export const day = (ts: number) => date(ts).toLocaleDateString([], { day: 'numeric', month: 'short' });

/** 6 Oct 2026 */
export const fullDay = (ts: number) => date(ts).toLocaleDateString([], { day: 'numeric', month: 'short', year: 'numeric' });

/** Wed 7 Oct; the year is added when it is not this year: Mon 22 Sep 2025. */
export function weekday(ts: number, now = Date.now() / 1000) {
  const d = date(ts);
  const year = d.getFullYear() === date(now).getFullYear() ? undefined : 'numeric';
  return d.toLocaleDateString([], { weekday: 'short' }) + ' ' + d.toLocaleDateString([], { day: 'numeric', month: 'short', year });
}

/** The heading of one day of a feed: Today, Wed 7 Oct · Yesterday, Tue 6 Oct · Mon 5 Oct. */
export function dayHeading(ts: number, now = Date.now() / 1000) {
  const yesterday = date(now);
  yesterday.setDate(yesterday.getDate() - 1);
  const d = date(ts).toDateString();
  const prefix = d === date(now).toDateString() ? 'Today, ' : d === yesterday.toDateString() ? 'Yesterday, ' : '';
  return prefix + weekday(ts, now);
}

/** $0.32; four decimals below one cent so tiny model costs do not read as zero. */
export const money = (usd: number) => '$' + usd.toFixed(usd > 0 && usd < 0.01 ? 4 : 2);

/** 0.91, the way the prototype shows model confidence. */
export const confidence = (c: number) => c.toFixed(2);
