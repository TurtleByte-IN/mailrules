import * as rulesApi from '../api/rules';

export const rules = $state<{ list: rulesApi.Rule[] }>({ list: [] });

export async function load() {
  rules.list = await rulesApi.list();
}

/** `list` with the item at `from` moved to `to`; the same array when nothing would move. */
export function moved<T>(list: T[], from: number, to: number): T[] {
  if (from === to || [from, to].some((i) => i < 0 || i >= list.length)) return list;
  const out = list.slice();
  out.splice(to, 0, ...out.splice(from, 1));
  return out;
}

/** Moves a rule to position `to` (0 = checked first) and saves the order. */
export async function move(id: string, to: number) {
  const list = rules.list;
  const next = moved(list, list.findIndex((r) => r.id === id), to);
  if (next !== list) rules.list = await rulesApi.reorder(next.map((r) => r.id));
}

export async function edit(id: string, patch: rulesApi.RulePatch) {
  const saved = await rulesApi.patch(id, patch);
  rules.list = rules.list.map((r) => (r.id === id ? saved : r));
}

export async function remove(id: string) {
  await rulesApi.remove(id);
  rules.list = rules.list.filter((r) => r.id !== id);
}

/** Saves new rules at the bottom of the list. A rule that trashes mail must be surer before it acts. */
export async function add(inputs: Parameters<typeof rulesApi.batch>[0]) {
  const added = await rulesApi.batch(
    inputs.map((r) => ({ ...r, min_confidence: r.min_confidence ?? (rulesApi.isTrash(r.actions) ? 0.9 : 0.75) })),
  );
  rules.list.push(...added);
  return added;
}
