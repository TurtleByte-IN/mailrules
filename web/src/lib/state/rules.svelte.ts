import { subscribe } from '../api/events';
import * as rulesApi from '../api/rules';
import { flash } from './toast.svelte';

// `loaded` and `error` keep "no rules yet" apart from "not fetched yet" and "the fetch failed".
export const rules = $state<{ list: rulesApi.Rule[]; loaded: boolean; error: string }>({ list: [], loaded: false, error: '' });

export async function load() {
  try {
    rules.list = await rulesApi.list();
    rules.loaded = true;
    rules.error = '';
  } catch (e) {
    rules.error = (e as Error).message;
  }
}

/** `list` with the item at `from` moved to `to`; the same array when nothing would move. */
export function moved<T>(list: T[], from: number, to: number): T[] {
  if (from === to || [from, to].some((i) => i < 0 || i >= list.length)) return list;
  const out = list.slice();
  out.splice(to, 0, ...out.splice(from, 1));
  return out;
}

/** Moves a rule to position `to` (0 = checked first) and saves the order; a refused order is put back. */
export async function move(id: number, to: number) {
  const before = rules.list;
  const next = moved(before, before.findIndex((r) => r.id === id), to);
  if (next === before) return;
  rules.list = next;
  try {
    rules.list = await rulesApi.reorder(next.map((r) => r.id));
  } catch (e) {
    rules.list = before;
    flash((e as Error).message);
  }
}

export async function edit(id: number, patch: rulesApi.RulePatch) {
  const saved = await rulesApi.patch(id, patch);
  rules.list = rules.list.map((r) => (r.id === id ? saved : r));
  return saved;
}

export async function remove(id: number) {
  await rulesApi.remove(id);
  rules.list = rules.list.filter((r) => r.id !== id);
}

/** Saves new rules at the bottom of the list. A rule that trashes mail must be surer before it acts. */
export async function add(inputs: rulesApi.RuleInput[]) {
  const added = await rulesApi.batch(
    inputs.map((r) => (rulesApi.isTrash(r.actions) ? { ...r, min_confidence: r.min_confidence ?? 0.9 } : r)),
  );
  rules.list.push(...added);
  return added;
}

/** Uploads a YAML rules file; the answer carries every rule after the import. */
export async function importFile(file: Blob) {
  const result = await rulesApi.importYaml(file);
  rules.list = result.items;
  return result;
}

/** Undoes what the rule did since midnight, local time. */
export const undoToday = (id: number) => rulesApi.undo(id, Math.floor(new Date().setHours(0, 0, 0, 0) / 1000));

// Another tab, the CLI or an import changed the rules.
subscribe('rules.changed', load);
