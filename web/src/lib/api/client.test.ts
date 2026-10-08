import { afterEach, describe, expect, it, vi } from 'vitest';
import { api, ApiError, query, setUnauthorizedHandler } from './client';

function respond(status: number, body?: unknown) {
  const fetchMock = vi.fn(async (_url: string, _init: RequestInit) =>
    new Response(body === undefined ? null : JSON.stringify(body), { status }),
  );
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

afterEach(() => {
  vi.unstubAllGlobals();
  document.cookie = 'mailrules_csrf=; max-age=0';
  setUnauthorizedHandler(() => {});
});

describe('api', () => {
  it('sends the CSRF cookie as a header on non-GET only', async () => {
    document.cookie = 'mailrules_csrf=tok%3D1';
    const f = respond(200, {});

    await api('GET', '/rules');
    await api('POST', '/rules/reorder', { ids: [] });

    const [get, post] = f.mock.calls.map((c) => c[1].headers as Record<string, string>);
    expect(f.mock.calls[0][0]).toBe('/api/rules');
    expect(get['X-CSRF-Token']).toBeUndefined();
    expect(post['X-CSRF-Token']).toBe('tok=1');
    expect(f.mock.calls[1][1].body).toBe('{"ids":[]}');
  });

  it('returns undefined for 204', async () => {
    respond(204);
    expect(await api('DELETE', '/rules/1')).toBeUndefined();
  });

  it.each([
    ['API error body', 422, { error: { code: 'rule_invalid', message: 'bad op', path: 'conditions.all[0].op' } }, 'rule_invalid', 'bad op', 'conditions.all[0].op'],
    ['non-JSON body', 502, undefined, 'http_error', '', undefined],
  ])('maps %s to ApiError', async (_name, status, body, code, message, path) => {
    respond(status, body);
    const err = await api('GET', '/x').catch((e) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect(err).toMatchObject({ status, code, message, path });
  });

  it('calls the unauthorized handler on 401', async () => {
    respond(401, { error: { code: 'unauthorized', message: 'sign in' } });
    const handler = vi.fn();
    setUnauthorizedHandler(handler);
    await expect(api('GET', '/auth/me')).rejects.toMatchObject({ status: 401 });
    expect(handler).toHaveBeenCalledExactlyOnceWith('unauthorized', undefined);
  });
});

it.each([
  [{}, ''],
  [{ cursor: '', limit: undefined, rule: null }, ''],
  [{ limit: 50, account: 2, status: 'review' }, '?limit=50&account=2&status=review'],
])('query(%j) = %j', (params, out) => expect(query(params)).toBe(out));
