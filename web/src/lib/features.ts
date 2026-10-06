// P2 features stay off until their backend milestone exists (docs/frontend-plan.md → Feature flags).
// This is the only place one is switched on.
export const features = {
  digest: false,
  notifications: false,
  timedActions: false,
  draftReplies: false,
  billing: false,
} as const;

export type Feature = keyof typeof features;
