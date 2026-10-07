// Appearance: System (the device's light or dark setting), Light or Dark. The choice lives in
// this browser's localStorage only. public/theme.js applies it before first paint; this module
// applies it again when it changes, and keeps System in step with the device. Keep the two in step.

export type Choice = 'system' | 'light' | 'dark';
type Scheme = 'light' | 'dark';

export const KEY = 'mailrules.theme';
// The browser bar colour for <meta name="theme-color">; a meta tag cannot read a CSS token.
const BAR: Record<Scheme, string> = { light: '#16130F', dark: '#171411' };
const DEVICE_DARK = '(prefers-color-scheme: dark)';

export function stored(): Choice {
  let v: string | null = null;
  try {
    v = localStorage.getItem(KEY);
  } catch {
    // Storage can be off (private mode); System then.
  }
  return v === 'light' || v === 'dark' ? v : 'system';
}

export const scheme = (c: Choice, deviceDark: boolean): Scheme => (c === 'system' ? (deviceDark ? 'dark' : 'light') : c);

export function apply(c: Choice = stored()) {
  const s = scheme(c, window.matchMedia(DEVICE_DARK).matches);
  document.documentElement.dataset.theme = s;
  document.querySelector('meta[name="theme-color"]')?.setAttribute('content', BAR[s]);
}

export function choose(c: Choice) {
  try {
    if (c === 'system') localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, c);
  } catch {
    // Not saved; it still applies until the page is reloaded.
  }
  apply(c);
}

/** Re-applies System whenever the device switches between light and dark. Returns the unsubscribe. */
export function followDevice() {
  const q = window.matchMedia(DEVICE_DARK);
  const changed = () => stored() === 'system' && apply('system');
  q.addEventListener('change', changed);
  return () => q.removeEventListener('change', changed);
}
