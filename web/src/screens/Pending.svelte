<script lang="ts">
  import { router } from 'svelte-spa-router';
  import { settings } from '../lib/state/settings.svelte';
  import { screensFor } from '../routes';

  const screen = $derived(screensFor(settings.value.features).find((s) => s.path === router.location));
</script>

<h1 class="text-2xl">{screen?.label ?? 'Not found'}</h1>
<p class="mt-2 text-secondary">
  {screen ? 'This screen is not built yet.' : 'No screen lives at this address.'}
</p>
