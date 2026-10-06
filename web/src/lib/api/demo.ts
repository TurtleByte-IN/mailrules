// Stand-in for endpoints the backend has not published in api/openapi.yaml yet.
// A resource module built on this returns demo data held in memory; the shell shows a
// "Demo data" banner while any are left. When an endpoint lands, its function body
// becomes an api() call and its types come from schema.d.ts. See docs/frontend-plan.md.
// Copies through JSON, not structuredClone: callers pass Svelte $state proxies, which
// structuredClone refuses, and the daemon's replies are JSON anyway.
export const fake = <T>(value: T): Promise<T> =>
  Promise.resolve(value === undefined ? value : JSON.parse(JSON.stringify(value)));

let n = 0;
export const fakeId = (prefix: string) => `${prefix}${Date.now().toString(36)}${n++}`;
