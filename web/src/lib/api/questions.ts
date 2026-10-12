import { api } from './client';
import type { components } from './schema';

export type MoveQuestion = components['schemas']['MoveQuestion'];
export type Answer = 'yes' | 'no';

// Answered only by a build with a module that learns from the emails people move in other
// mail apps; settings.features.move_questions says so.
export const list = () => api<{ questions: MoveQuestion[] }>('GET', '/activity/questions');
export const answer = (id: number, a: Answer) => api<{ question: MoveQuestion; answer: Answer }>('POST', `/activity/questions/${id}/answer`, { answer: a });
