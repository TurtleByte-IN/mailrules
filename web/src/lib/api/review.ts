import type { FixRequest, Ok } from './activity';
import { api, query } from './client';

export const list = (cursor?: string | null) => api<Ok<'listReview'>>('GET', '/review' + query({ cursor }));
/** rule_id null keeps the message in the Inbox. */
export const resolve = (id: number, fix: FixRequest) => api<Ok<'resolveReview'>>('POST', `/review/${id}/resolve`, fix);
