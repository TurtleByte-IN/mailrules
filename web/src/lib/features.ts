// Later-phase features stay off until their backend exists.
// This is the only place one is switched on. A feature a module provides is not here: the
// daemon reports it in GET /api/settings `features` (notifications, suggest).
export const features = {
  timedActions: false,
  draftReplies: false,
  billing: false,
  unsubscribe: false,
} as const;

export type Feature = keyof typeof features;
