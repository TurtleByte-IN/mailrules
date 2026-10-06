import { ApiError } from '../api/client';
import * as composeApi from '../api/compose';
import type { RuleInput } from '../api/rules';
import * as templatesApi from '../api/templates';
import { add, rules } from './rules.svelte';
import { flash } from './toast.svelte';

/** `refused` is the daemon's sentence when the last save was refused because of this draft; `refusedName` says it was about the name. */
export type Draft = composeApi.Draft & { rejected: boolean; refused?: string; refusedName?: boolean };

// `text` lives here, not in the screen, so it survives leaving Add rules and another
// screen can hand over a starting sentence. `needsModel` is the daemon's own sentence when
// it has no model to compose with; Add rules shows it with a link to Settings.
export const compose = $state<{ text: string; drafts: Draft[]; unparsed: string[]; busy: boolean; templates: templatesApi.Template[]; needsModel: string }>({
  text: '',
  drafts: [],
  unparsed: [],
  busy: false,
  templates: [],
  needsModel: '',
});

const fail = (e: unknown) => flash((e as Error).message);

/** Turns the text into drafts. A failure leaves the text and any earlier drafts as they were. */
export async function optimize() {
  const text = compose.text.trim();
  if (!text) return flash('Type or dictate at least one rule first');
  compose.busy = true;
  compose.needsModel = '';
  try {
    const result = await composeApi.compose({ text });
    compose.drafts = result.rules.map((d) => ({ ...d, rejected: false }));
    compose.unparsed = result.unparsed;
    if (!compose.drafts.length) flash("Couldn't find a rule in that. Try describing which emails and where they go.");
  } catch (e) {
    if (e instanceof ApiError && e.code === 'no_composer_model') compose.needsModel = e.message;
    else fail(e);
  } finally {
    compose.busy = false;
  }
}

/** A draft the daemon found a problem in cannot be saved as it is. */
export const savable = (d: Draft) => !d.rejected && !d.errors.length;

const toInput = ({ name, said, intent, conditions, exceptions, actions, account_id, stack, model, min_confidence, new_folders }: Draft): RuleInput => ({
  name,
  said,
  intent,
  conditions,
  exceptions,
  actions,
  account_id,
  stack,
  model,
  min_confidence,
  new_folders,
});

/**
 * Saves every draft not skipped. Returns the saved rules, or nothing when nothing was saved.
 * A refusal that names one of the rules sent ("rules[1].name") goes on that draft.
 */
export async function saveAll() {
  const keep = compose.drafts.filter(savable);
  if (!keep.length) return flash('Nothing to save: every draft is skipped');
  for (const d of compose.drafts) Object.assign(d, { refused: '', refusedName: false });
  try {
    const added = await add(keep.map(toInput));
    compose.drafts = [];
    compose.unparsed = [];
    compose.text = '';
    flash(added.length + (added.length === 1 ? ' rule saved and live' : ' rules saved and live'));
    return added;
  } catch (e) {
    // The daemon counts the rules as sent, so skipped drafts are not in its index.
    const at = e instanceof ApiError && /^rules\[(\d+)\]\./.exec(e.path ?? '');
    if (at && keep[+at[1]]) Object.assign(keep[+at[1]], { refused: e.message, refusedName: /\.name$/.test(e.path ?? '') });
    else fail(e);
  }
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
  try {
    if (await addTemplatesByName([t.name])) flash(t.name + ' added and live');
  } catch (e) {
    fail(e);
  }
}
