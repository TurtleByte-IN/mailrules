import { afterEach, expect, it, vi } from 'vitest';
import { ApiError } from './client';
import { suggest, type SuggestResult } from './suggest';

function respond(status: number, body: BodyInit | null, type = 'application/json') {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) => new Response(body, { status, headers: { 'Content-Type': type } }));
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => vi.unstubAllGlobals());

const result: SuggestResult = { suggestions: [], scanned: 40, total: 40, matched: 40, groups: 6, requests: 1, tokens: 900, cost_usd: 0.001, notes: [] };
const req = { account_id: 7, folder: 'INBOX', since: null, limit: 200, samples: 5, body: 'none' } as const;

it('follows a streamed scan: progress events, then the result', async () => {
  const stream =
    'event: progress\ndata: {"phase":"reading","read":0,"total":40}\n\n' +
    'event: progress\ndata: {"phase":"asking","read":40,"total":40}\n\n' +
    `event: done\ndata: ${JSON.stringify(result)}\n\n`;
  const chunks = [stream.slice(0, 25), stream.slice(25, 80), stream.slice(80)].map((c) => new TextEncoder().encode(c));
  const f = respond(200, new ReadableStream({ start: (c) => (chunks.forEach((x) => c.enqueue(x)), c.close()) }), 'text/event-stream');
  const progress = vi.fn();

  expect(await suggest(req, progress)).toEqual(result);
  expect(progress.mock.calls.map((c) => c[0].phase)).toEqual(['reading', 'asking']);
  expect(f.mock.calls[0][0]).toBe('/api/rules/suggest');
  expect(f.mock.calls[0][1]).toMatchObject({ method: 'POST', headers: { Accept: 'application/json, text/event-stream' } });
  expect(JSON.parse(f.mock.calls[0][1].body as string)).toEqual(req);
});

it('throws the error a streamed scan carries when it fails midway', async () => {
  const error = { code: 'suggest_failed', message: 'The model stopped answering.' };
  respond(200, `event: progress\ndata: {"phase":"reading","read":3,"total":40}\n\nevent: error\ndata: ${JSON.stringify({ error })}\n\n`, 'text/event-stream');
  const thrown = await suggest(req).catch((e) => e);
  expect(thrown).toBeInstanceOf(ApiError);
  expect(thrown).toMatchObject(error);
});

it('fails a streamed scan that ends without a result', async () => {
  respond(200, 'event: progress\ndata: {"phase":"reading","read":1,"total":40}\n\n', 'text/event-stream');
  await expect(suggest(req)).rejects.toMatchObject({ code: 'stream_ended', message: 'The scan stopped before it finished.' });
});

it('reads a scan answered in one body, and throws a scan that cannot start', async () => {
  respond(200, JSON.stringify(result));
  expect(await suggest(req)).toEqual(result);

  respond(409, JSON.stringify({ error: { code: 'no_composer_model', message: 'Choose a model in Settings.' } }));
  await expect(suggest(req)).rejects.toMatchObject({ status: 409, code: 'no_composer_model' });
});
