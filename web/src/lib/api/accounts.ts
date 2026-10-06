// DEMO: GET /api/accounts is not in api/openapi.yaml yet (backend M7).
import { fake } from './demo';

export interface Account {
  id: string;
  email: string;
  provider: string;
  status: 'live' | 'reconnecting' | 'error';
}

const accounts: Account[] = [
  { id: 'acc1', email: 'me@icloud.com', provider: 'iCloud', status: 'live' },
  { id: 'acc2', email: 'work@fastmail.com', provider: 'Fastmail', status: 'live' },
];

export const list = () => fake(accounts);
