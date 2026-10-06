import { api, ApiError, query } from './client';
import type { components, operations } from './schema';

type S = components['schemas'];

export type Rule = S['Rule'];
export type RuleInput = S['RuleInput'];
export type RulePatch = S['RulePatch'];
export type Condition = S['Condition'];
export type Action = S['RuleAction'];
export type ImportResult = S['ImportResult'];
export type TestResult = S['TestResult'];
export type TestProgress = S['TestProgress'];
export type UndoResult = S['UndoResult'];

// ponytail: reads one level of a condition tree; a group nested inside a group shows as an
// empty condition. Walk the tree when the composer or an imported file starts nesting them.
export const leaves = (t: Condition): Condition[] => t.all ?? t.any ?? (t.field ? [t] : []);
export const isTrash = (actions: Action[]) => actions.some((a) => a.type === 'trash');

export const list = async () => (await api<S['RuleList']>('GET', '/rules')).items;
export const patch = async (id: number, p: RulePatch) => (await api<S['RuleEnvelope']>('PATCH', `/rules/${id}`, p)).rule;
export const remove = (id: number) => api<void>('DELETE', `/rules/${id}`);

/** Sets the order rules are checked in; `ids` is every rule's id. Returns the list in that order. */
export const reorder = async (ids: number[]) => (await api<S['RuleList']>('POST', '/rules/reorder', { ids })).items;

/** Saves new rules; the only route that creates rules besides import. */
export const batch = async (rules: RuleInput[]) =>
  (await api<operations['createRules']['responses'][201]['content']['application/json']>('POST', '/rules/batch', { rules } satisfies S['RuleBatchRequest'])).items;

/** Undoes everything the rule did at or after `since` (unix seconds). */
export const undo = (id: number, since: number) => api<UndoResult>('POST', `/rules/${id}/undo` + query({ since }));

// api() sends and reads JSON only; the YAML file and the tester's event stream need the
// response itself. The CSRF cookie and the error shape are client.ts's, repeated here.
// ponytail: a 401 on these three does not reach client.ts's unauthorized handler; move this
// into client.ts when it grows a raw request.
async function send(method: string, path: string, headers: Record<string, string>, body?: BodyInit) {
  const csrf = document.cookie.split('; ').find((c) => c.startsWith('mailrules_csrf='))?.slice(15) ?? '';
  if (method !== 'GET') headers['X-CSRF-Token'] = decodeURIComponent(csrf);
  const res = await fetch('/api' + path, { method, headers, credentials: 'same-origin', body });
  if (res.ok) return res;
  const err = (await res.json().catch(() => null))?.error;
  throw new ApiError(res.status, err?.code ?? 'http_error', err?.message ?? res.statusText, err?.path);
}

/** Every rule as the YAML rules file. */
export const exportYaml = async () => (await send('GET', '/rules/export', { Accept: 'application/yaml' })).blob();

/** Uploads a YAML rules file. Nothing is stored unless every rule in it is valid. */
export const importYaml = async (file: Blob): Promise<ImportResult> =>
  (await send('POST', '/rules/import', { Accept: 'application/json', 'Content-Type': 'application/yaml' }, file)).json();

/**
 * Runs saved (`rule_ids`) or draft (`rules`) rules over the account's recent mail without acting.
 * Up to 200 emails the daemon answers in one body; above that it streams, and `onProgress`
 * is called as it goes.
 */
export async function test(req: Omit<S['TestRequest'], 'folder'>, onProgress?: (p: TestProgress) => void): Promise<TestResult> {
  const res = await send('POST', '/rules/test', { Accept: 'application/json, text/event-stream', 'Content-Type': 'application/json' }, JSON.stringify(req));
  if (!res.headers.get('Content-Type')?.startsWith('text/event-stream')) return res.json();

  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) throw new ApiError(502, 'stream_ended', 'The test stopped before it finished.');
    buffer += decoder.decode(value, { stream: true });
    for (let end; (end = buffer.indexOf('\n\n')) >= 0; buffer = buffer.slice(end + 2)) {
      const frame = buffer.slice(0, end);
      const event = /^event: ?(.*)$/m.exec(frame)?.[1];
      const data = /^data: ?(.*)$/m.exec(frame)?.[1];
      if (event === 'done') return JSON.parse(data!);
      if (event === 'progress') onProgress?.(JSON.parse(data!));
    }
  }
}
