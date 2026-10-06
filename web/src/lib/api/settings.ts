// DEMO: GET/PATCH /api/settings are not in api/openapi.yaml yet (backend M7).
import { fake } from './demo';

export interface Settings {
  /** On by default: the daemon logs what it would do and changes nothing. */
  dryRun: boolean;
}

let settings: Settings = { dryRun: true };

export const get = () => fake(settings);
export const patch = (p: Partial<Settings>) => fake((settings = { ...settings, ...p }));
