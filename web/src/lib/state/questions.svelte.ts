import { subscribe } from '../api/events';
import * as questionsApi from '../api/questions';
import { settings } from './settings.svelte';
import { flash } from './toast.svelte';

// The questions Activity asks about the emails the person moved in other mail apps. Only a
// build with a module that learns from those moves asks any (settings.value.features.move_questions);
// without one nothing here is fetched or shown.
export const questions = $state<{ list: questionsApi.MoveQuestion[]; error: string }>({ list: [], error: '' });

export async function load() {
  if (!settings.value.features.move_questions) return;
  try {
    questions.list = (await questionsApi.list()).questions;
    questions.error = '';
  } catch (e) {
    questions.error = (e as Error).message;
  }
}

/** Answers one question: once answered it leaves the list and is never asked again. */
export async function answer(q: questionsApi.MoveQuestion, a: questionsApi.Answer) {
  let failed = '';
  try {
    await questionsApi.answer(q.id, a);
    if (a === 'yes') flash(q.inbox ? `Email from ${q.from} now stays in Inbox.` : `Email from ${q.from} now goes to ${q.folder}.`);
  } catch (e) {
    failed = (e as Error).message;
  }
  // Answered here, elsewhere, or not: the daemon's list is the truth.
  await load();
  if (failed) questions.error = failed;
}

// A question was asked, brought up to date or answered elsewhere.
subscribe('questions.changed', load);
