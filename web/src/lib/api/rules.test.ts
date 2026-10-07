import { afterEach, expect, it, vi } from 'vitest';
import { ApiError } from './client';
import { exportYaml, importYaml, limitRefused, test, testRefused, undo, type TestResult } from './rules';

function respond(status: number, body: BodyInit | null, type = 'application/json') {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => new Response(body, { status, headers: { 'Content-Type': type } }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = 'mailrules_csrf=; max-age=0';
});

const done: TestResult = { results: [], tested: 200, matched: 12, model_calls: 3, cost_usd: 0.00006 };

it('uploads the YAML file as the body, with the CSRF header', async () => {
  document.cookie = 'mailrules_csrf=tok%3D1';
  const file = new Blob(['rules: []']);
  const f = respond(200, JSON.stringify({ items: [], created: 0, updated: 0 }));

  expect(await importYaml(file)).toEqual({ items: [], created: 0, updated: 0 });
  const [url, init] = f.mock.calls[0];
  expect(url).toBe('/api/rules/import');
  expect(init).toMatchObject({ method: 'POST', body: file, headers: { 'Content-Type': 'application/yaml', 'X-CSRF-Token': 'tok=1' } });
});

it('turns a refused import into the API error, every problem in the message', async () => {
  const message = 'Rule 1 ("Bad"): A rule needs conditions, an intent, or both.\nRule 2 ("Worse"): A rule needs at least one action.';
  respond(400, JSON.stringify({ error: { code: 'rule_invalid', message, path: 'conditions' } }));
  await expect(importYaml(new Blob(['x']))).rejects.toMatchObject({ status: 400, code: 'rule_invalid', message, path: 'conditions' });
});

it('downloads the rules file without a CSRF header', async () => {
  const f = respond(200, 'rules:\n    - id: Food\n', 'application/yaml');
  expect(await (await exportYaml()).text()).toBe('rules:\n    - id: Food\n');
  expect(f.mock.calls[0][0]).toBe('/api/rules/export');
  expect(f.mock.calls[0][1].headers).toEqual({ Accept: 'application/yaml' });
});

it('undoes a rule since a time given in the query', async () => {
  const f = respond(200, JSON.stringify({ batch: {}, undone: 2, failed: 0 }));
  await undo(7, 1791273148);
  expect(f.mock.calls[0][0]).toBe('/api/rules/7/undo?since=1791273148');
});

it('reads a test answered in one body', async () => {
  const f = respond(200, JSON.stringify(done));
  expect(await test({ account_id: 1, rule_ids: [4], limit: 200 })).toEqual(done);
  expect(f.mock.calls[0][1].body).toBe('{"account_id":1,"rule_ids":[4],"limit":200}');
});

it('asks for an event stream, so the daemon reports progress at any size', async () => {
  const f = respond(200, JSON.stringify(done));
  await test({ account_id: 1, limit: 20 });
  expect(f.mock.calls[0][1].headers).toMatchObject({ Accept: 'application/json, text/event-stream' });
});

it('follows a streamed test: progress events, then the result', async () => {
  // Cut mid-frame, the way a network delivers it.
  const stream = `event: progress\ndata: {"done":0,"total":500,"model_calls":0}\n\nevent: progress\ndata: {"done":500,"total":500,"model_calls":3}\n\nevent: done\ndata: ${JSON.stringify(done)}\n\n`;
  const chunks = [stream.slice(0, 30), stream.slice(30, 95), stream.slice(95)].map((c) => new TextEncoder().encode(c));
  respond(200, new ReadableStream({ start: (c) => (chunks.forEach((x) => c.enqueue(x)), c.close()) }), 'text/event-stream');
  const progress = vi.fn();

  expect(await test({ account_id: 1, limit: 500 }, progress)).toEqual(done);
  expect(progress.mock.calls).toEqual([[{ done: 0, total: 500, model_calls: 0 }], [{ done: 500, total: 500, model_calls: 3 }]]);
});

it('throws a refused number as the daemon says it, whatever the client accepts', async () => {
  const message = 'The limit must be between 1 and 2000.';
  respond(400, JSON.stringify({ error: { code: 'invalid_input', message, path: 'limit' } }));
  const thrown = await test({ account_id: 1, limit: 9999 }).catch((e) => e);
  expect(thrown).toMatchObject({ status: 400, message });
  expect(limitRefused(thrown)).toBe(true);
  expect(limitRefused(new ApiError(400, 'invalid_input', 'x', 'folder'))).toBe(false);
  expect(limitRefused(new Error('x'))).toBe(false);
});

it('fails a streamed test that ends without a result', async () => {
  respond(200, 'event: progress\ndata: {"done":1,"total":500}\n\n', 'text/event-stream');
  await expect(test({ account_id: 1, limit: 500 })).rejects.toMatchObject({ code: 'stream_ended' });
});

it('throws the error a streamed test carries when it fails midway', async () => {
  const error = { code: 'test_failed', message: 'The mailbox stopped answering.' };
  respond(200, `event: progress\ndata: {"done":25,"total":500}\n\nevent: error\ndata: ${JSON.stringify({ error })}\n\n`, 'text/event-stream');
  const thrown = await test({ account_id: 1, limit: 500 }).catch((e) => e);
  expect(thrown).toBeInstanceOf(ApiError);
  expect(thrown).toMatchObject(error);
});

it.each([
  [409, 'no_composer_model', true],
  [409, 'account_offline', true],
  [502, 'model_error', false],
])('a test answered %i %s is shown in place of the result: %s', async (status, code, inline) => {
  respond(status, JSON.stringify({ error: { code, message: 'Why.' } }));
  expect(testRefused(await test({ account_id: 1, limit: 200 }).catch((e) => e))).toBe(inline);
});
