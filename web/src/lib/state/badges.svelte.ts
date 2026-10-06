// Counts shown beside nav items, keyed by route path. The state module that owns the
// number writes it here, so the shell does not import every resource.
export const badges = $state<Record<string, number>>({});
