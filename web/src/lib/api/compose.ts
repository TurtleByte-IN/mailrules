import { api } from './client';
import type { components, operations } from './schema';

/** A rule the composer proposes. Nothing is saved until the user approves it. */
export type Draft = components['schemas']['RuleDraft'];
export type ComposeResult = components['schemas']['ComposeResult'];

/** What "Use an example" types into the box. */
export const sample =
  "Bank statements and credit card bills go to Finance and mark them read. Archive LinkedIn emails about who viewed my profile. Cold sales pitches from people I've never emailed go to Trash.";

/** Turns typed or dictated text (up to 4,000 characters) into draft rules. */
export const compose = (req: components['schemas']['ComposeRequest']) => api<ComposeResult>('POST', '/rules/compose', req);

/** Re-optimizes one saved rule from its original wording plus `text`. Returns a draft; save it with a patch. */
export const recompose = async (id: number, text: string) =>
  (await api<operations['recomposeRule']['responses'][200]['content']['application/json']>('POST', `/rules/${id}/compose`, { text })).rule;
