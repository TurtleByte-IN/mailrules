import { folders as listFolders } from '../api/accounts';
import type { Folder } from '../api/cleanup';
import { ApiError } from '../api/client';
import * as suggestApi from '../api/suggest';
import { scopeProblem, startScope, toRequest, type Scope } from '../scope';
import { toInput } from './compose.svelte';
import { add } from './rules.svelte';
import { flash } from './toast.svelte';

// idle → scanning → ready (cards shown) → created. A failed scan goes back to where it was, so
// earlier cards stay. Changing the scope or the controls clears nothing: only a new scan replaces
// the cards.
type Phase = 'idle' | 'scanning' | 'ready' | 'created';

/**
 * A suggestion as a card: `ticked` says it is created with "Create N selected". `refused` is the
 * daemon's sentence when the last create was refused because of this card; `refusedName` says it
 * was about the name.
 */
export type Card = suggestApi.RuleSuggestion & { ticked: boolean; refused?: string; refusedName?: boolean };

export const suggest = $state<{
  phase: Phase;
  scope: Scope;
  /** Folders of the chosen mailbox. */
  folders: Folder[];
  /** "Up to" N samples per sender, or all of them. */
  samplesMode: 'upto' | 'all';
  /** The "Up to" box; null is an empty box. */
  upTo: number | null;
  body: suggestApi.SuggestBody;
  /** The daemon's sentence when it refused the number of samples; goes when the number changes. */
  samplesRefused: string;
  /** How far the running scan is; null until the mail is listed. */
  progress: suggestApi.SuggestProgress | null;
  result: suggestApi.SuggestResult | null;
  cards: Card[];
  /** The daemon's own sentence when it has no model to suggest with; shown with a link to Settings. */
  needsModel: string;
  /** Rules the last create made. */
  created: number;
}>({
  phase: 'idle',
  scope: startScope(),
  folders: [],
  samplesMode: 'upto',
  upTo: suggestApi.SUGGEST_START.samples,
  body: suggestApi.SUGGEST_START.body,
  samplesRefused: '',
  progress: null,
  result: null,
  cards: [],
  needsModel: '',
  created: 0,
});

/** What is wrong with the samples box, in a sentence; empty when nothing is, and for "All of them". */
export function samplesProblem() {
  if (suggest.samplesRefused) return suggest.samplesRefused;
  if (suggest.samplesMode === 'all') return '';
  return suggest.upTo !== null && Number.isInteger(suggest.upTo) && suggest.upTo >= 1 ? '' : 'Give a whole number of samples, 1 or more.';
}

// Without the list only Inbox is offered, so a failure here costs the Archive choice and nothing else.
async function loadFolders() {
  const id = suggest.scope.accountId;
  suggest.folders = [];
  const found = await listFolders(Number(id)).catch(() => []);
  if (id === suggest.scope.accountId) suggest.folders = found;
}

/** The scope cannot change under a running scan; another mailbox starts on its Inbox. */
export function setScope(patch: Partial<Scope>) {
  if (suggest.phase === 'scanning') return;
  const other = patch.accountId !== undefined && patch.accountId !== suggest.scope.accountId;
  Object.assign(suggest.scope, patch);
  if (other) {
    suggest.scope.folder = 'INBOX';
    loadFolders();
  }
}

/** Sets the samples choice or box; a refusal of the old number goes with it. */
export function setSamples(patch: Partial<Pick<typeof suggest, 'samplesMode' | 'upTo'>>) {
  Object.assign(suggest, patch, { samplesRefused: '' });
}

/** Runs a scan; its cards replace any earlier ones. Refused while one runs or a box is wrong. */
export async function scan() {
  if (suggest.phase === 'scanning' || !suggest.scope.accountId || scopeProblem(suggest.scope) || samplesProblem()) return;
  const before = suggest.phase === 'ready' ? 'ready' : 'idle';
  const req: suggestApi.SuggestRequest = { ...toRequest(suggest.scope), samples: suggest.samplesMode === 'all' ? 'all' : suggest.upTo!, body: suggest.body };
  suggest.phase = 'scanning';
  suggest.progress = null;
  suggest.needsModel = '';
  try {
    const result = await suggestApi.suggest(req, (p) => (suggest.progress = p));
    suggest.result = result;
    // A rule that trashes mail is only created when the user ticks it; one in error never is.
    suggest.cards = result.suggestions.map((s) => ({ ...s, ticked: !s.errors.length && !s.trashes }));
    suggest.phase = 'ready';
  } catch (e) {
    suggest.phase = before;
    if (e instanceof ApiError && e.code === 'no_composer_model') suggest.needsModel = e.message;
    else if (e instanceof ApiError && e.code === 'invalid_input' && e.path === 'samples') suggest.samplesRefused = e.message;
    else flash((e as Error).message);
  } finally {
    suggest.progress = null;
  }
}

/** The cards "Create N selected" sends. */
export const selected = () => suggest.cards.filter((c) => c.ticked && !c.errors.length);

/**
 * Creates the ticked cards' rules the way Describe saves drafts. A refusal that names one of the
 * rules sent ("rules[1].name") goes on that card. Creating sorts nothing already in the mailbox.
 */
export async function createSelected() {
  const sent = selected();
  if (!sent.length) return;
  for (const c of suggest.cards) Object.assign(c, { refused: '', refusedName: false });
  try {
    const added = await add(sent.map(toInput));
    suggest.created = added.length;
    suggest.cards = [];
    suggest.result = null;
    suggest.phase = 'created';
  } catch (e) {
    // The daemon counts the rules as sent, so unticked cards are not in its index.
    const at = e instanceof ApiError && /^rules\[(\d+)\]\./.exec(e.path ?? '');
    if (at && sent[+at[1]]) Object.assign(sent[+at[1]], { refused: e.message, refusedName: /\.name$/.test(e.path ?? '') });
    else flash((e as Error).message);
  }
}

/** Throws the cards away. */
export function discard() {
  suggest.cards = [];
  suggest.result = null;
  suggest.phase = 'idle';
}

/** Back to the controls after a create. */
export function again() {
  suggest.created = 0;
  suggest.phase = 'idle';
}
