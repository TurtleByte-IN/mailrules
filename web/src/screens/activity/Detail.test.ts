import { cleanup, render, screen } from '@testing-library/svelte';
import { afterEach, expect, it } from 'vitest';
import { detail } from '../../lib/state/activity.fixtures';
import { settings } from '../../lib/state/settings.svelte';
import Detail from './Detail.svelte';

afterEach(() => {
  cleanup();
  settings.value.retention_days = 0;
});

it.each([
  [90, 'Preview (snippet kept for 90 days)'],
  [1, 'Preview (snippet kept for 1 day)'],
  [3650, 'Preview (snippet kept for 3650 days)'],
  [0, 'Preview (snippet kept for the retention period set in Settings)'],
])('labels the preview with the retention setting %s', (days, label) => {
  settings.value.retention_days = days;
  render(Detail, { message: detail(), id: 'd' });
  expect(screen.getByText(label)).toBeTruthy();
});
