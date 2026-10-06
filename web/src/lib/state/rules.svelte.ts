import * as rulesApi from '../api/rules';

export const rules = $state<{ list: rulesApi.Rule[] }>({ list: [] });

export async function load() {
  rules.list = await rulesApi.list();
}
