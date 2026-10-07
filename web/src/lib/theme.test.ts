import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import bootScript from '../../public/theme.js?raw';
import { apply, choose, followDevice, KEY, stored } from './theme';

// A device whose light or dark setting the test can flip, as the OS would.
function device(dark: boolean) {
  const listeners = new Set<() => void>();
  const query = {
    get matches() {
      return dark;
    },
    addEventListener: (_: string, f: () => void) => listeners.add(f),
    removeEventListener: (_: string, f: () => void) => listeners.delete(f),
  };
  vi.stubGlobal('matchMedia', () => query);
  return (next: boolean) => {
    dark = next;
    listeners.forEach((f) => f());
  };
}

const applied = () => [document.documentElement.dataset.theme, document.querySelector('meta[name="theme-color"]')?.getAttribute('content')];

beforeEach(() => {
  localStorage.clear();
  delete document.documentElement.dataset.theme;
  document.head.innerHTML = '<meta name="theme-color" content="">';
});
afterEach(() => vi.unstubAllGlobals());

const cases = [
  { saved: null, deviceDark: false, want: ['light', '#16130F'] },
  { saved: null, deviceDark: true, want: ['dark', '#171411'] },
  { saved: 'light', deviceDark: true, want: ['light', '#16130F'] },
  { saved: 'dark', deviceDark: false, want: ['dark', '#171411'] },
  { saved: 'purple', deviceDark: true, want: ['dark', '#171411'] },
];

describe.each([
  { who: 'the script before first paint', run: () => new Function(bootScript)() },
  { who: 'the app', run: () => apply() },
])('$who', ({ run }) => {
  it.each(cases)('saved $saved on a device that is dark: $deviceDark', ({ saved, deviceDark, want }) => {
    if (saved) localStorage.setItem(KEY, saved);
    device(deviceDark);
    run();
    expect(applied()).toEqual(want);
  });
});

it('System follows the device live; Light and Dark do not', () => {
  const flip = device(false);
  const stop = followDevice();
  choose('system');
  flip(true);
  expect(applied()).toEqual(['dark', '#171411']);
  flip(false);
  expect(applied()).toEqual(['light', '#16130F']);

  choose('dark');
  flip(true);
  flip(false);
  expect(applied()).toEqual(['dark', '#171411']);
  stop();
});

it('keeps the choice for the next visit, and System clears it', () => {
  device(false);
  choose('dark');
  expect(stored()).toBe('dark');
  choose('system');
  expect(localStorage.getItem(KEY)).toBeNull();
  expect(stored()).toBe('system');
});
