// Plan and billing: only in a build that sells plans (`features.billing`); elsewhere the routes answer 404.
import { api } from './client';
import type { components, paths } from './schema';

export type PlanInfo = components['schemas']['PlanInfo'];
export type PlanChoice = components['schemas']['PlanChoice'];
export type PlanTerms = components['schemas']['PlanTerms'];
export type CompanySignIn = NonNullable<PlanInfo['company_sign_in']>;
export type Interval = PlanChoice['interval'];
export type Checkout = paths['/api/plan/checkout']['post']['responses']['200']['content']['application/json'];

export const get = () => api<PlanInfo>('GET', '/plan');
/** What the browser needs to open the billing service's checkout; for a tenant that has not subscribed. */
export const checkout = (c: PlanChoice) => api<Checkout>('POST', '/plan/checkout', c);
/** Asks the billing service to move a subscribed tenant; the change lands with the `plan.changed` event. */
export const change = (c: PlanChoice) => api<PlanInfo>('POST', '/plan/change', c);
/** The billing service's customer portal: cancel, payment method, invoices. */
export const portal = () => api<{ url: string }>('GET', '/plan/portal');
/**
 * Owner only: asks the billing service to add the company sign-in add-on to the Team
 * subscription, or remove it; the change lands with the `plan.changed` event.
 */
export const companySignIn = (addOn: boolean) => api<PlanInfo>('POST', '/plan/company-sign-in', { add_on: addOn });
