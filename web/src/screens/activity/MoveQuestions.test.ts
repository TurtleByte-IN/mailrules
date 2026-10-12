import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { MoveQuestion } from '../../lib/api/questions';
import { load, questions } from '../../lib/state/questions.svelte';
import { settings } from '../../lib/state/settings.svelte';
import { toast } from '../../lib/state/toast.svelte';
import MoveQuestions from './MoveQuestions.svelte';

const inbox: MoveQuestion = { id: 1, account_id: 3, mailbox: 'priya@icloud.com', from: 'news@shop.example', inbox: true, folder: 'INBOX', emails: 2, asked_at: 1791276732 };
const later: MoveQuestion = { ...inbox, id: 2, from: 'deals@shop.example', inbox: false, folder: 'Later', emails: 1 };

/** Answers each "METHOD /path" with [status, body]; returns the mock to inspect the calls. */
function serve(routes: Record<string, [number, unknown]>) {
  const f = vi.fn(async (url: string, init: RequestInit) => {
    const [status, body] = routes[`${init.method} ${url}`] ?? [404, { error: { code: 'not_found', message: 'No such route in this test.' } }];
    return new Response(JSON.stringify(body), { status });
  });
  vi.stubGlobal('fetch', f);
  return f;
}

beforeEach(() => {
  settings.value.features.move_questions = true;
  questions.list = [inbox, later];
  questions.error = '';
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  settings.value.features.move_questions = false;
  questions.list = [];
});

it('asks once per sender and destination, Yes or No', () => {
  render(MoveQuestions);
  const first = screen.getByRole('group', { name: 'You moved 2 emails from this sender to Inbox. Always keep them in Inbox?' });
  expect(within(first).getByText('news@shop.example · on priya@icloud.com')).toBeTruthy();
  expect(screen.getByRole('group', { name: 'You moved 1 email from this sender to Later. Always move them to Later?' })).toBeTruthy();
});

it('Yes learns the sender, says so, and the question goes', async () => {
  const f = serve({
    'POST /api/activity/questions/1/answer': [200, { question: inbox, answer: 'yes' }],
    'GET /api/activity/questions': [200, { questions: [later] }],
  });
  render(MoveQuestions);
  const first = screen.getByRole('group', { name: /to Inbox/ });
  await fireEvent.click(within(first).getByRole('button', { name: 'Yes' }));
  await vi.waitFor(() => expect(screen.queryByRole('group', { name: /to Inbox/ })).toBeNull());
  expect(JSON.parse(f.mock.calls[0][1].body as string)).toEqual({ answer: 'yes' });
  expect(toast.text).toBe('Email from news@shop.example now stays in Inbox.');
  expect(screen.getByRole('group', { name: /to Later/ })).toBeTruthy();
});

it('No closes the question without learning anything', async () => {
  const f = serve({
    'POST /api/activity/questions/2/answer': [200, { question: later, answer: 'no' }],
    'GET /api/activity/questions': [200, { questions: [inbox] }],
  });
  toast.text = '';
  render(MoveQuestions);
  await fireEvent.click(within(screen.getByRole('group', { name: /to Later/ })).getByRole('button', { name: 'No' }));
  await vi.waitFor(() => expect(screen.queryByRole('group', { name: /to Later/ })).toBeNull());
  expect(JSON.parse(f.mock.calls[0][1].body as string)).toEqual({ answer: 'no' });
  expect(toast.text).toBe('');
});

it('says why an answer did not go through, and keeps the question', async () => {
  serve({
    'POST /api/activity/questions/1/answer': [500, { error: { code: 'internal', message: 'Something went wrong.' } }],
    'GET /api/activity/questions': [200, { questions: [inbox, later] }],
  });
  render(MoveQuestions);
  await fireEvent.click(within(screen.getByRole('group', { name: /to Inbox/ })).getByRole('button', { name: 'Yes' }));
  await vi.waitFor(() => expect(screen.getByRole('alert').textContent).toBe('Something went wrong.'));
  expect(screen.getByRole('group', { name: /to Inbox/ })).toBeTruthy();
});

it('shows nothing when there is nothing to ask', () => {
  questions.list = [];
  render(MoveQuestions);
  expect(screen.queryByRole('region')).toBeNull();
});

it('fetches nothing in a build without the module that learns from moves', async () => {
  settings.value.features.move_questions = false;
  questions.list = [];
  const f = serve({});
  await load();
  expect(f).not.toHaveBeenCalled();
});
