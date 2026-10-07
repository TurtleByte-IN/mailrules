import { afterAll, describe, expect, it } from 'vitest';
import { settings } from './settings.svelte';
import { limitProblem, testLimit } from './testlimit.svelte';

// The daemon's own numbers, made different from today's 200 and 2000 so the tester is seen to follow them.
const daemon = { test_default: 300, test_max: 1500, check_max: 1500 };

describe('before the settings are loaded', () => {
  it('has an empty box and asks only for a whole number from 1', () => {
    expect(testLimit.value).toBeNull();
    expect(limitProblem(5000)).toBe('');
    expect(limitProblem(0)).toBe('Give a whole number of emails, 1 or more.');
    expect(limitProblem(null)).toBe('Give a whole number of emails, 1 or more.');
  });
});

describe('with the daemon’s limits', () => {
  const placeholder = settings.value.limits;
  afterAll(() => Object.assign(settings.value, { limits: placeholder }));

  it('starts at what the daemon reads when it is not told, until the user changes it', () => {
    Object.assign(settings.value, { limits: daemon });
    expect(testLimit.value).toBe(300);
    testLimit.value = 20;
    Object.assign(settings.value, { limits: { ...daemon, test_default: 400 } });
    expect(testLimit.value).toBe(20);
    testLimit.value = null;
    expect(testLimit.value).toBeNull();
    Object.assign(settings.value, { limits: daemon });
  });

  it.each([
    [1, ''],
    [300, ''],
    [1500, ''],
    [0, 'The limit must be between 1 and 1500.'],
    [1501, 'The limit must be between 1 and 1500.'],
    [-5, 'The limit must be between 1 and 1500.'],
    [12.5, 'The limit must be between 1 and 1500.'],
    [null, 'The limit must be between 1 and 1500.'],
  ])('checks %s', (n, problem) => expect(limitProblem(n)).toBe(problem));
});
