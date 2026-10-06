import * as accountsApi from '../api/accounts';

export const accounts = $state<{ list: accountsApi.Account[] }>({ list: [] });

export async function load() {
  accounts.list = await accountsApi.list();
}
