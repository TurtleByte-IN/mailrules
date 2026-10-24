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

export type Recomposed = operations['recomposeRule']['responses'][200]['content']['application/json'];
export type MatchShift = components['schemas']['MatchShift'];

/**
 * Rewrites one saved rule from its original wording, how it behaved and `text`; empty `text`
 * improves it from how it behaved alone. Returns a draft, what changed and why, and the test
 * emails it starts or stops taking; save the draft with a patch.
 */
export const recompose = (id: number, text: string) => api<Recomposed>('POST', `/rules/${id}/compose`, { text });
