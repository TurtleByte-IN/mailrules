import { describe, expect, it } from 'vitest';
import * as rulesApi from '../api/rules';
import { add, edit, load, move, moved, remove, rules } from './rules.svelte';

describe('moved', () => {
  const list = ['a', 'b', 'c', 'd'];
  it.each([
    ['up one', 2, 1, 'acbd'],
    ['down one', 0, 1, 'bacd'],
    ['dragged to the top', 3, 0, 'dabc'],
    ['dragged to the bottom', 0, 3, 'bcda'],
    ['onto itself', 1, 1, 'abcd'],
    ['first one up', 0, -1, 'abcd'],
    ['last one down', 3, 4, 'abcd'],
    ['unknown item', -1, 2, 'abcd'],
  ])('%s', (_, from, to, want) => {
    expect(moved(list, from, to).join('')).toBe(want);
    expect(list.join('')).toBe('abcd');
  });
});

const ids = () => rules.list.map((r) => r.id);

describe('rules state', () => {
  it('keeps a new order across a reload', async () => {
    await load();
    const [first, second, ...rest] = ids();

    await move(second, 0);
    expect(ids()).toEqual([second, first, ...rest]);

    await load();
    expect(ids()).toEqual([second, first, ...rest]);
  });

  it('edits, adds and removes through the API', async () => {
    await load();
    const id = ids()[0];

    await edit(id, { enabled: false, actions: [{ type: 'trash' }] });
    const [added] = await add([
      { name: 'New', said: 's', intent: 'spam', conditions: {}, exceptions: {}, actions: [{ type: 'trash' }], account_id: null, stack: false, model: null, min_confidence: null },
    ]);
    expect(rules.list[0]).toMatchObject({ enabled: false, trash: true });
    expect(rules.list.at(-1)).toMatchObject({ id: added.id, enabled: true, hits: 0, trash: true, min_confidence: 0.9 });

    await remove(id);
    expect(ids()).not.toContain(id);
    expect((await rulesApi.list()).map((r) => r.id)).toEqual(ids());
  });
});
