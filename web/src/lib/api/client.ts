// The only module that calls fetch. Owns the CSRF header and the error shape
// (docs/backend-plan.md → HTTP API, Conventions).

// Cookie name set by the daemon (internal/api/auth.go, csrfCookie).
const CSRF_COOKIE = 'mailrules_csrf';

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    /** Field the error points at, e.g. "conditions.all[0].op". */
    readonly path?: string,
  ) {
    super(message);
    this.name = 'ApiError';
  }
}

/**
 * An AI feature the daemon cannot run until something is set in Settings: the rule composer
 * model, or the Claude workspace a key that covers a whole organisation needs. Its message
 * says what, so it is shown with a way to Settings.
 */
export const needsSettings = (e: unknown): e is ApiError =>
  e instanceof ApiError && (e.code === 'no_composer_model' || e.code === 'anthropic_workspace_needed');

let onUnauthorized = (_code: string) => {};

/** Called once by the auth state; runs on any 401 with the error code so the app can show Setup or Login. */
export function setUnauthorizedHandler(fn: (code: string) => void) {
  onUnauthorized = fn;
}

function csrfToken(): string {
  const hit = document.cookie.split('; ').find((c) => c.startsWith(CSRF_COOKIE + '='));
  return hit ? decodeURIComponent(hit.slice(CSRF_COOKIE.length + 1)) : '';
}

/**
 * One request to the daemon with the CSRF header; resolves with the response when it is 2xx
 * and throws ApiError otherwise. For bodies that are not JSON (YAML, a stream); JSON calls use api().
 */
export async function request(method: string, path: string, headers: Record<string, string> = {}, body?: BodyInit): Promise<Response> {
  if (method !== 'GET') headers['X-CSRF-Token'] = csrfToken();
  const res = await fetch('/api' + path, { method, headers, credentials: 'same-origin', body });
  if (res.ok) return res;

  const err = (await res.json().catch(() => null))?.error;
  if (res.status === 401) onUnauthorized(err?.code ?? 'unauthenticated');
  throw new ApiError(res.status, err?.code ?? 'http_error', err?.message ?? res.statusText, err?.path);
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await request(method, path, headers, body === undefined ? undefined : JSON.stringify(body));
  return (res.status === 204 ? undefined : await res.json()) as T;
}

/**
 * POSTs `body` as JSON to a route that can report progress. Asking for an event stream makes the
 * daemon send `progress` events (passed to `onProgress`), then one `done` event with the result or one
 * `error` event, which throws as ApiError. A request that cannot start is answered as plain JSON: an
 * error throws as any request does, a result is returned as it is. A stream that closes without
 * either throws `ended` as its message.
 */
export async function stream<T, P>(path: string, body: unknown, onProgress: ((p: P) => void) | undefined, ended: string): Promise<T> {
  const res = await request('POST', path, { Accept: 'application/json, text/event-stream', 'Content-Type': 'application/json' }, JSON.stringify(body));
  if (!res.headers.get('Content-Type')?.startsWith('text/event-stream')) return res.json();

  const reader = res.body!.getReader();
  const decoder = new TextDecoder();
  let buffer = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) throw new ApiError(502, 'stream_ended', ended);
    buffer += decoder.decode(value, { stream: true });
    for (let end; (end = buffer.indexOf('\n\n')) >= 0; buffer = buffer.slice(end + 2)) {
      const frame = buffer.slice(0, end);
      const event = /^event: ?(.*)$/m.exec(frame)?.[1];
      const data = /^data: ?(.*)$/m.exec(frame)?.[1];
      if (event === 'done') return JSON.parse(data!);
      if (event === 'progress') onProgress?.(JSON.parse(data!));
      if (event === 'error') {
        const { error }: { error: { code: string; message: string; path?: string } } = JSON.parse(data!);
        throw new ApiError(502, error.code, error.message, error.path);
      }
    }
  }
}

/** Builds "?a=1&b=2" from the set values; empty string when there are none. */
export function query(params: Record<string, string | number | boolean | null | undefined>) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') q.set(k, String(v));
  const s = q.toString();
  return s ? '?' + s : '';
}
