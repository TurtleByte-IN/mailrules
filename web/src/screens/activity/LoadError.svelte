<script lang="ts">
  let { message, retry }: { message: string; retry: () => void } = $props();
</script>

<div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-3 py-2 text-trash">
  <span class="min-w-0 break-words">{message}</span>
  <button type="button" class="btn" onclick={retry}>Retry</button>
</div>
