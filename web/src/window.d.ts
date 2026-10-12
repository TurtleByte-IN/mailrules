// What the billing module's checkout script (POST /api/plan/checkout `script`) puts on the window.
interface Window {
  mailrulesCheckout?: {
    /** Opens the billing service's checkout; `done` is called once it is completed or closed. */
    open(checkout: Record<string, unknown>, done: (result: 'completed' | 'closed') => void): void;
  };
}
