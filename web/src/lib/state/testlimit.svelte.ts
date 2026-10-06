import { TEST_LIMIT } from '../api/rules';

/**
 * How many emails the rule tester reads. One number for both screens that start a test, kept while the
 * app is open and forgotten when it is closed. null is an empty box.
 */
export const testLimit = $state<{ value: number | null }>({ value: TEST_LIMIT.default });

/** What is wrong with a number of emails to test, in the daemon's own words; empty when nothing is. */
export const limitProblem = (n: number | null) =>
  n !== null && Number.isInteger(n) && n >= 1 && n <= TEST_LIMIT.max ? '' : `The limit must be between 1 and ${TEST_LIMIT.max}.`;
