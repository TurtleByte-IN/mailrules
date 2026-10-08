// Later-phase features stay off until their backend exists.
// This is the only place one is switched on.
export const features = {
  notifications: false,
  timedActions: false,
  draftReplies: false,
  billing: false,
  unsubscribe: false,
} as const;

export type Feature = keyof typeof features;
