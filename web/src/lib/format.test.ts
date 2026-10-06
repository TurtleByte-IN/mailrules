import { expect, it } from 'vitest';
import { confidence, money } from './format';

it.each([
  [0, '$0.00'],
  [0.0042, '$0.0042'],
  [0.32, '$0.32'],
  [12.5, '$12.50'],
])('money(%s) = %s', (usd, out) => expect(money(usd)).toBe(out));

it('confidence keeps two decimals', () => expect(confidence(0.9)).toBe('0.90'));
