// Company sign-in (SAML or OIDC): only while `features.company_sign_in` is on, for an Owner or
// Admin; elsewhere the routes answer 404, 403 plan_required or 403 role_required.
import { api } from './client';
import type { paths } from './schema';

export type Status = paths['/api/company-sign-in']['get']['responses']['200']['content']['application/json']['status'];
export type Intent = 'sso' | 'domain_verification';

/** How the team's connection stands at the sign-in service, as its module reports it. */
export const status = () => api<{ status: Status }>('GET', '/company-sign-in');
/** A one-time, short-lived link to the sign-in service's admin portal; open it at once. */
export const portal = (intent: Intent) => api<{ url: string }>('POST', '/company-sign-in/portal', { intent });
