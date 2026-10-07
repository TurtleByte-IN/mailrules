import { stream } from './client';
import type { components } from './schema';

type S = components['schemas'];

export type SuggestRequest = S['SuggestRequest'];
export type SuggestResult = S['SuggestResult'];
export type RuleSuggestion = S['RuleSuggestion'];
export type SuggestProgress = S['SuggestProgress'];
export type SuggestBody = NonNullable<SuggestRequest['body']>;

/**
 * Where the "Samples per sender" and "Body sent to the AI" controls start: the daemon's own defaults
 * for a request that leaves them out. Starting values awaiting Tilak's sign-off. A Go test reads this
 * line and fails when it differs from the daemon's defaults, so keep it on one line, written as it is.
 */
export const SUGGEST_START = { samples: 5, body: 'none' } as const;

/**
 * Scans the chosen mail and has the AI suggest rules; nothing is saved or moved. `onProgress` is
 * called once the mail is listed, then as the scan reads, asks and merges. A scan that cannot start
 * (no model, mailbox offline, a refused field) throws as any request does; one that fails midway
 * throws the error the stream carried.
 */
export const suggest = (req: SuggestRequest, onProgress?: (p: SuggestProgress) => void) =>
  stream<SuggestResult, SuggestProgress>('/rules/suggest', req, onProgress, 'The scan stopped before it finished.');
