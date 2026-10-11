import { api } from './client';
import type { components } from './schema';

type S = components['schemas'];

export type NotificationList = S['NotificationList'];
export type Kind = S['NotificationKind'];
export type Destination = S['NotificationDestination'];
export type Delivery = S['NotificationDelivery'];
export type DestinationInput = S['NotificationDestinationInput'];
export type Created = S['NotificationDestinationCreated'];

// Answered only by a build with a notifications module; settings.features.notifications says so.
export const list = () => api<NotificationList>('GET', '/notifications');
export const create = (input: DestinationInput) => api<Created>('POST', '/notifications/destinations', input);
export const remove = (id: number) => api<void>('DELETE', `/notifications/destinations/${id}`);
