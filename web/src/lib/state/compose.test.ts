import { beforeEach, expect, it } from 'vitest';
import { sample } from '../api/compose';
import { answer, compose, optimize, saveAll } from './compose.svelte';
import { load, rules } from './rules.svelte';
import { toast } from './toast.svelte';

beforeEach(async () => {
  await load();
  compose.text = sample;
  await optimize();
});

it('turns text into drafts and saves only the ones not skipped', async () => {
  const before = rules.list.length;
  expect(compose.drafts.map((d) => d.name)).toEqual(['Finance statements', 'LinkedIn profile views', 'Cold sales']);

  compose.drafts[1].rejected = true;
  answer(compose.drafts[2], 'Move to Sales');
  const added = await saveAll();

  expect(added?.map((r) => r.name)).toEqual(['Finance statements', 'Cold sales']);
  expect(rules.list.slice(before)).toEqual(added);
  expect(added?.[1]).toMatchObject({ actions: [{ type: 'move', folder: 'Sales' }], trash: false, min_confidence: 0.75 });
  expect(added?.[1]).not.toHaveProperty('question');
  expect(compose).toMatchObject({ drafts: [], text: '' });
  expect(toast.text).toBe('2 rules saved and live');
});

it('saves nothing when every draft is skipped', async () => {
  const before = rules.list.length;
  for (const d of compose.drafts) d.rejected = true;

  expect(await saveAll()).toBeUndefined();
  expect(rules.list).toHaveLength(before);
  expect(compose.drafts).toHaveLength(3);
  expect(toast.text).toBe('Nothing to save: every draft is skipped');
});

it.each([
  ['', 0, 'Type or dictate at least one rule first'],
  ['ok', 0, "Couldn't find a rule in that. Try describing which emails and where they go."],
  ['Mail from swiggy.in goes to Food and mark it read. Delete lottery spam', 2, ''],
])('optimize(%j) gives %i drafts', async (text, n, flashed) => {
  toast.text = '';
  compose.drafts = [];
  compose.text = text;
  await optimize();
  expect(compose.drafts).toHaveLength(n);
  expect(toast.text).toBe(flashed);
});

it('reads sender, folder and actions out of plain sentences', async () => {
  compose.text = 'Mail from swiggy.in goes to Food and mark it read. Delete lottery spam';
  await optimize();
  expect(compose.drafts[0]).toMatchObject({
    name: 'Food',
    conditions: { all: [{ field: 'from_domain', op: 'in', value: ['swiggy.in'] }] },
    actions: [{ type: 'move', folder: 'Food' }, { type: 'read' }],
    new_folders: [],
  });
  expect(compose.drafts[1].actions).toEqual([{ type: 'trash' }]);
});
