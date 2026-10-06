import { expect, it } from 'vitest';
import { TEST_LIMIT } from '../api/rules';
import { limitProblem, testLimit } from './testlimit.svelte';

it('starts at what the daemon reads when it is not told', () => {
  expect(testLimit.value).toBe(TEST_LIMIT.default);
  expect(TEST_LIMIT.default).toBe(200);
});

it.each([
  [1, ''],
  [200, ''],
  [2000, ''],
  [0, 'The limit must be between 1 and 2000.'],
  [2001, 'The limit must be between 1 and 2000.'],
  [-5, 'The limit must be between 1 and 2000.'],
  [12.5, 'The limit must be between 1 and 2000.'],
  [null, 'The limit must be between 1 and 2000.'],
])('checks %s', (n, problem) => expect(limitProblem(n)).toBe(problem));
