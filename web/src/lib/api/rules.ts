import { api, ApiError, needsSettings, query, request, stream } from './client';
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
/** A test the daemon will not run as things stand; its message says what to do, so it is shown where the result would be. */
export const testRefused = (e: unknown): e is ApiError => needsSettings(e) || (e instanceof ApiError && e.code === 'account_offline');

/** The daemon refused the number of emails to test, and says why. */
export const limitRefused = (e: unknown): e is ApiError => e instanceof ApiError && e.code === 'invalid_input' && e.path === 'limit';

/**
 * How many emails a test reads: what it reads when `limit` is left out, and the most the daemon allows
 * (composer.DefaultLimit and composer.MaxLimit; `TestRequest.limit.maximum` in api/openapi.yaml).
 * The generated types carry no numeric bounds, so the numbers are written here, once; a Go test
 * (TestTestLimitsAgreeEverywhere) fails when they differ from the daemon's. The daemon still checks.
 */
export const TEST_LIMIT = { default: 200, max: 2000 } as const;

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

// api() sends and reads JSON only; the YAML file needs the response itself, and the tester's
// event stream is read by stream().

/** Every rule as the YAML rules file. */
export const exportYaml = async () => (await request('GET', '/rules/export', { Accept: 'application/yaml' })).blob();

/** Uploads a YAML rules file. Nothing is stored unless every rule in it is valid. */
export const importYaml = async (file: Blob): Promise<ImportResult> =>
  (await request('POST', '/rules/import', { Accept: 'application/json', 'Content-Type': 'application/yaml' }, file)).json();

/**
 * Runs saved (`rule_ids`) or draft (`rules`) rules over the account's recent mail without acting.
 * Asking for an event stream makes the daemon report progress at any size: `onProgress` is called
 * with 0 of N once the mail is listed, then as it goes. A run that cannot start (no such folder, no
 * model, a refused limit) is answered as plain JSON and throws as any request does; one that fails
 * midway throws the error the stream carried.
 */
export const test = (req: S['TestRequest'], onProgress?: (p: TestProgress) => void) =>
  stream<TestResult, TestProgress>('/rules/test', req, onProgress, 'The test stopped before it finished.');
