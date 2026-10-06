// DEMO: GET/PATCH /api/rules are not in api/openapi.yaml yet (backend M7).
import { fake } from './demo';

export interface Rule {
  id: string;
  name: string;
  enabled: boolean;
  /** True when the rule's action moves mail to Trash. */
  trash: boolean;
}

const rules: Rule[] = [
  { id: 'r1', name: 'Food orders', enabled: true, trash: false },
  { id: 'r2', name: 'Login codes', enabled: true, trash: false },
  { id: 'r3', name: 'Recruiters', enabled: true, trash: false },
  { id: 'r4', name: 'Scams', enabled: true, trash: true },
  { id: 'r5', name: 'Newsletters', enabled: true, trash: false },
  { id: 'r6', name: 'Orders', enabled: true, trash: false },
];

export const list = () => fake(rules);
