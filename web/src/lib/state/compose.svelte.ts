import * as composeApi from '../api/compose';
import * as templatesApi from '../api/templates';
import { add, rules } from './rules.svelte';
import { flash } from './toast.svelte';

export type Draft = composeApi.Draft & { rejected: boolean };

// `text` lives here, not in the screen, so it survives leaving Add rules and another
// screen can hand over a starting sentence.
export const compose = $state<{ text: string; drafts: Draft[]; busy: boolean; templates: templatesApi.Template[] }>({
  text: '',
  drafts: [],
  busy: false,
  templates: [],
});

export async function optimize() {
  const text = compose.text.trim();
  if (!text) return flash('Type or dictate at least one rule first');
  compose.busy = true;
  try {
    compose.drafts = (await composeApi.compose(text)).map((d) => ({ ...d, rejected: false }));
  } finally {
    compose.busy = false;
  }
  if (!compose.drafts.length) flash("Couldn't find a rule in that. Try describing which emails and where they go.");
}

/** Answers a draft's question; the answer decides what the rule does. */
export function answer(d: Draft, option: string) {
  d.answer = option;
  const folder = option.startsWith('Move to ') ? option.slice(8) : '';
  if (option === 'Trash') d.actions = [{ type: 'trash' }];
  else if (option === 'Keep in Inbox') d.actions = [{ type: 'keep' }];
  else if (folder) d.actions = [{ type: 'move', folder }];
  else return;
  d.new_folders = folder ? [folder] : [];
}

/** Saves every draft not skipped. Returns the saved rules, or nothing when there was nothing to save. */
export async function saveAll() {
  const keep = compose.drafts.filter((d) => !d.rejected);
  if (!keep.length) return flash('Nothing to save: every draft is skipped');
  const added = await add(
    keep.map(({ rejected, question, options, answer, conflicts, match_count, samples, ...rule }) => rule),
  );
  compose.drafts = [];
  compose.text = '';
  flash(added.length + (added.length === 1 ? ' rule saved and live' : ' rules saved and live'));
  return added;
}

export async function loadTemplates() {
  compose.templates = await templatesApi.list();
}

/** Adds the named templates as rules, skipping any whose name is already a rule. Returns how many were added. */
export async function addTemplatesByName(names: string[]) {
  if (!compose.templates.length) await loadTemplates();
  const picked = compose.templates.filter((t) => names.includes(t.name) && !rules.list.some((r) => r.name === t.name));
  if (picked.length)
    await add(
      picked.map((t) => ({ name: t.name, said: 'Template: ' + t.name, intent: t.intent, conditions: t.conditions, exceptions: {}, actions: t.actions, account_id: null, stack: false, model: null, min_confidence: null })),
    );
  return picked.length;
}

export async function addTemplate(t: templatesApi.Template) {
  if (await addTemplatesByName([t.name])) flash(t.name + ' added and live');
}
