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

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = { Accept: 'application/json' };
  if (method !== 'GET') headers['X-CSRF-Token'] = csrfToken();
  if (body !== undefined) headers['Content-Type'] = 'application/json';

  const res = await fetch('/api' + path, {
    method,
    headers,
    credentials: 'same-origin',
    body: body === undefined ? undefined : JSON.stringify(body),
  });

  if (res.ok) return (res.status === 204 ? undefined : await res.json()) as T;

  const err = (await res.json().catch(() => null))?.error;
  if (res.status === 401) onUnauthorized(err?.code ?? 'unauthenticated');
  throw new ApiError(res.status, err?.code ?? 'http_error', err?.message ?? res.statusText, err?.path);
}
