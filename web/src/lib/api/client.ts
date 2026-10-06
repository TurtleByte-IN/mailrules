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

/** True when the daemon has the route in its contract but has not built it yet (501). */
export const notBuilt = (e: unknown) => e instanceof ApiError && e.code === 'not_implemented';

/** Builds "?a=1&b=2" from the set values; empty string when there are none. */
export function query(params: Record<string, string | number | boolean | null | undefined>) {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') q.set(k, String(v));
  const s = q.toString();
  return s ? '?' + s : '';
}
