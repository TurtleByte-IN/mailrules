// The billing module's checkout script, loaded once from this server as a <script src> element.
const loading = new Map<string, Promise<void>>();

/** Loads the script at `src` once; later calls for the same path wait for that one load. A failed load can be tried again. */
export function loadScript(src: string): Promise<void> {
  const known = loading.get(src);
  if (known) return known;
  const p = new Promise<void>((resolve, reject) => {
    const el = document.createElement('script');
    el.src = src;
    el.onload = () => resolve();
    el.onerror = () => {
      el.remove();
      loading.delete(src);
      reject(new Error("The checkout didn't load. Try again."));
    };
    document.head.append(el);
  });
  loading.set(src, p);
  return p;
}

/** Opens checkout, once its script is loaded; resolves with how it ended. */
export async function openCheckout(script: string, checkout: Record<string, unknown>): Promise<'completed' | 'closed'> {
  await loadScript(script);
  const c = window.mailrulesCheckout;
  if (!c) throw new Error("The checkout didn't load. Try again.");
  return new Promise((resolve) => c.open(checkout, resolve));
}
