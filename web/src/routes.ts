import type { Component } from 'svelte';
import { features, type Feature } from './lib/features';
import Accounts from './screens/Accounts.svelte';
import Activity from './screens/Activity.svelte';
import Cleanup from './screens/Cleanup.svelte';
import Compose from './screens/Compose.svelte';
import Review from './screens/Review.svelte';
import Rules from './screens/Rules.svelte';
import Senders from './screens/Senders.svelte';
import Settings from './screens/Settings.svelte';
import Usage from './screens/Usage.svelte';
import Overview from './screens/Overview.svelte';
import Pending from './screens/Pending.svelte';
import Setup from './screens/Setup.svelte';

export interface Screen {
  path: string;
  label: string;
  /** SVG path data, 24x24 stroke icon, from the prototype nav. */
  icon: string;
  component: Component<any>;
  feature?: Feature;
}

// Order and labels follow the prototype nav. Plan and billing is P2: it keeps the
// stand-in until its backend exists.
const all: Screen[] = [
  { path: '/', label: 'Overview', icon: 'M3 3h7v9H3zM14 3h7v5h-7zM14 12h7v9h-7zM3 16h7v5H3z', component: Overview },
  { path: '/activity', label: 'Activity', icon: 'M3 12h4l3 8 4-16 3 8h4', component: Activity },
  { path: '/review', label: 'Needs review', icon: 'M5 21V4h12l-2 4 2 4H5', component: Review },
  { path: '/rules', label: 'Rules', icon: 'M9 6h11M9 12h11M9 18h11M4 6h.01M4 12h.01M4 18h.01', component: Rules },
  { path: '/senders', label: 'Senders', icon: 'M8 12a4 4 0 1 0 8 0 4 4 0 1 0-8 0M16 8v5a3 3 0 0 0 6 0v-1a10 10 0 1 0-4 8', component: Senders },
  { path: '/cleanup', label: 'Cleanup', icon: 'M3 4h18v4H3zM5 8v12h14V8M10 12h4', component: Cleanup },
  { path: '/compose', label: 'Add rules', icon: 'M12 5v14M5 12h14', component: Compose },
  { path: '/accounts', label: 'Mailboxes', icon: 'M5 5h14a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM3 7l9 6 9-6', component: Accounts },
  { path: '/usage', label: 'Usage', icon: 'M5 20V11M11 20V5M17 20v-7M3 20h18', component: Usage },
  { path: '/plan', label: 'Plan and billing', icon: 'M4 5h16a2 2 0 0 1 2 2v10a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V7a2 2 0 0 1 2-2zM2 10h20', component: Pending, feature: 'billing' },
  { path: '/settings', label: 'Settings', icon: 'M4 7h9M17 7h3M4 17h3M11 17h9M13 7a2 2 0 1 0 4 0 2 2 0 1 0-4 0M7 17a2 2 0 1 0 4 0 2 2 0 1 0-4 0', component: Settings },
];

export const screens = all.filter((s) => !s.feature || features[s.feature]);

export const routes: Record<string, Component<any>> = {
  ...Object.fromEntries(screens.map((s) => [s.path, s.component])),
  // The first-run guide: not in the nav, opened once right after the admin account is created.
  '/setup': Setup,
  '*': Pending,
};
