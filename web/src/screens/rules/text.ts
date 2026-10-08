// How a rule reads on screen: wording from the prototype over the rule shape in lib/api/rules.ts.
import { leaves, type Action, type Condition, type Rule } from '../../lib/api/rules';

/** The "more options" of a rule. */
export type Extras = Pick<Rule, 'account_id' | 'stack'>;

export interface FieldDef {
  label: string;
  type: 'text' | 'bool' | 'num';
  ph?: string;
  words: string;
}

export const fields: Record<string, FieldDef> = {
  from: { label: 'Sender address', type: 'text', ph: 'name@example.com', words: 'the sender is' },
  from_domain: { label: 'Sender domain', type: 'text', ph: 'swiggy.in, zomato.com', words: 'the sender domain' },
  to: { label: 'Sent to (alias)', type: 'text', ph: 'invoices@mydomain.com', words: 'it was sent to' },
  subject: { label: 'Subject', type: 'text', ph: 'invoice, receipt', words: 'the subject' },
  body: { label: 'Body text', type: 'text', ph: 'unsubscribe', words: 'the body' },
  list_id: { label: 'Mailing list (List-Id)', type: 'text', ph: 'news.example.com', words: 'the mailing list' },
  has_attachment: { label: 'Has an attachment', type: 'bool', words: 'it has an attachment' },
  attachment_ext: { label: 'Attachment type', type: 'text', ph: 'pdf, xlsx', words: 'the attachment type' },
  size_kb: { label: 'Size (KB)', type: 'num', ph: '500', words: 'its size in KB' },
  is_contact: { label: 'Sender is a contact', type: 'bool', words: 'the sender is a contact' },
  replied_before: { label: "I've replied to sender", type: 'bool', words: "I've replied to the sender" },
  is_bulk: { label: 'Bulk or newsletter', type: 'bool', words: "it's bulk mail" },
  dmarc: { label: 'DMARC result', type: 'text', ph: 'fail', words: 'the DMARC result' },
};

/** The chip beside a rule's name: who decides, and whether it costs anything. */
export function kind(r: { intent?: string | null; conditions?: Condition }) {
  if (!r.intent) return { label: 'Conditions · free', chip: 'chip chip-neutral' };
  return leaves(r.conditions ?? {}).length ? { label: 'Conditions + AI', chip: 'chip chip-both' } : { label: 'AI intent', chip: 'chip' };
}

const values = (c: Condition) => [c.value].flat().map(String);

/** from_domain in swiggy.in, zomato.com */
export const condText = (c: Condition) =>
  typeof c.value === 'boolean' ? `${c.field} = ${c.value}` : `${c.field} ${c.op} ${values(c).join(', ')}`;

const opWords: Record<string, string> = { in: 'is', contains_any: 'contains', matches: 'matches', gt: 'is more than', lt: 'is less than' };

function condWords(c: Condition) {
  const words = fields[c.field ?? '']?.words ?? c.field;
  if (typeof c.value === 'boolean') return c.value ? words : 'not (' + words + ')';
  const v = values(c);
  return `${words} ${opWords[c.op ?? ''] ?? c.op} ${v.length > 1 ? v.slice(0, -1).join(', ') + ' or ' + v.at(-1) : v[0]}`;
}

/** the sender domain is swiggy.in or zomato.com and it has an attachment */
export const treeWords = (t: Condition) => leaves(t).map(condWords).join(t.any ? ' or ' : ' and ');

const actionWords: Partial<Record<Action['type'], string>> = { archive: 'Archive', trash: 'Move to Trash', keep: 'Keep in Inbox', read: 'mark read' };

/**
 * Move to Food, mark read. `trashTo` names the folder a trash goes to when it is not the
 * server's Trash (`trash_to_folder`), for text about where mail goes or is about to go.
 */
export const actionsText = (actions: Action[], trashTo?: string) =>
  actions
    .map((a) => (a.type === 'move' ? 'Move to ' + (a.folder || '[folder]') : a.type === 'trash' && trashTo ? 'Move to ' + trashTo : (actionWords[a.type] ?? a.type)))
    .join(', ');

/** The tail after the actions. `only` is the mailbox address when the rule applies to one. */
export const extrasText = (x: Extras, only?: string) => (x.stack ? ' · stacks' : '') + (only ? ' · only ' + only : '');

/** One line for the rule list: conditions, then intent, then the exception. */
export function summary(r: Pick<Rule, 'intent' | 'conditions' | 'exceptions'>) {
  const conds = leaves(r.conditions).map(condText).join(r.conditions.any ? ' or ' : ' and ');
  const unless = treeWords(r.exceptions);
  return conds + (conds && r.intent ? ', about: ' : '') + r.intent + (unless ? ', unless ' + unless : '');
}

/** The decision models every install can pick by name; any other value is `name:model`, as the daemon's CheckModel accepts it. */
export const modelNames: Record<string, string> = { '': 'Default', jev: 'Jev', clef: 'Clef', anthropic: 'Claude Haiku 4.5' };

/** What a rule's model reads as on screen: the listed name, or the stored value as it is. */
export const modelLabel = (model: string) => modelNames[model] ?? model;

/** Which choice in the Model list a stored value is: a listed model, or one of the three that take a model name. */
export const modelChoice = (model: string): string => (model in modelNames ? model : model.startsWith('openai:') ? 'openai' : model.startsWith('ollama:') ? 'ollama' : 'other');
