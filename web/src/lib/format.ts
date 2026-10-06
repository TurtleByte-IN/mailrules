// Timestamps arrive as unix seconds and are shown in the browser's locale and zone.
const date = (ts: number) => new Date(ts * 1000);

/** 08:41 */
export const clock = (ts: number) => date(ts).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });

/** 6 Oct */
export const day = (ts: number) => date(ts).toLocaleDateString([], { day: 'numeric', month: 'short' });

/** $0.32; four decimals below one cent so tiny model costs do not read as zero. */
export const money = (usd: number) => '$' + usd.toFixed(usd > 0 && usd < 0.01 ? 4 : 2);

/** 0.91, the way the prototype shows model confidence. */
export const confidence = (c: number) => c.toFixed(2);
