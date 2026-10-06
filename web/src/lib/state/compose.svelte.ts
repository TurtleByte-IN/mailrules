import { notBuilt } from '../api/client';
import * as composeApi from '../api/compose';
import type { RuleInput } from '../api/rules';
import * as templatesApi from '../api/templates';
import { add, rules } from './rules.svelte';
import { flash } from './toast.svelte';

export type Draft = composeApi.Draft & { rejected: boolean };

// `text` lives here, not in the screen, so it survives leaving Add rules and another
// screen can hand over a starting sentence. `notBuilt` names the action that last got a
// 501 from the daemon; once set, Add rules shows the notice for the rest of the session.
export const compose = $state<{ text: string; drafts: Draft[]; unparsed: string[]; busy: boolean; templates: templatesApi.Template[]; notBuilt: string }>({
  text: '',
  drafts: [],
  unparsed: [],
  busy: false,
  templates: [],
  notBuilt: '',
});

/**
 * Runs an action whose route the daemon may not have built yet. A 501 puts up the notice
 * for `what`; any other failure is flashed. Resolves to undefined when it failed.
 */
export async function attempt<T>(what: string, action: () => Promise<T>): Promise<T | undefined> {
  try {
    return await action();
  } catch (e) {
    if (notBuilt(e)) compose.notBuilt = what;
    else flash((e as Error).message);
  }
}

export async function optimize() {
  const text = compose.text.trim();
  if (!text) return flash('Type or dictate at least one rule first');
  compose.busy = true;
  const result = await attempt('Turning text into rules', () => composeApi.compose({ text }));
  compose.busy = false;
  if (!result) return;
  compose.drafts = result.rules.map((d) => ({ ...d, rejected: false }));
  compose.unparsed = result.unparsed;
  if (!compose.drafts.length) flash("Couldn't find a rule in that. Try describing which emails and where they go.");
}

/** A draft the daemon found a problem in cannot be saved as it is. */
export const savable = (d: Draft) => !d.rejected && !d.errors.length;

const toInput = ({ name, said, intent, conditions, exceptions, actions, min_confidence, new_folders }: Draft): RuleInput => ({
  name,
  said,
  intent,
  conditions,
  exceptions,
  actions,
  min_confidence,
  new_folders,
  stack: false,
  enabled: true,
});

/** Saves every draft not skipped. Returns the saved rules, or nothing when nothing was saved. */
export async function saveAll() {
  const keep = compose.drafts.filter(savable);
  if (!keep.length) return flash('Nothing to save: every draft is skipped');
  const added = await attempt('Saving new rules', () => add(keep.map(toInput)));
  if (!added) return;
  compose.drafts = [];
  compose.unparsed = [];
  compose.text = '';
  flash(added.length + (added.length === 1 ? ' rule saved and live' : ' rules saved and live'));
  return added;
}

export async function loadTemplates() {
  compose.templates = await templatesApi.list();
}

/** Adds the named templates as rules, skipping any whose rule name is already taken. Returns how many were added. */
export async function addTemplatesByName(names: string[]) {
  if (!compose.templates.length) await loadTemplates();
  const picked = compose.templates.filter((t) => names.includes(t.name) && !rules.list.some((r) => r.name === t.rule.name));
  if (picked.length) await add(picked.map((t) => t.rule));
  return picked.length;
}

export async function addTemplate(t: templatesApi.Template) {
  if (await attempt('Saving new rules', () => addTemplatesByName([t.name]))) flash(t.name + ' added and live');
}
