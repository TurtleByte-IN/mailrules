<script lang="ts">
  let { what }: { what: string } = $props();
</script>

<div role="status" class="card bg-neutral px-4 py-3">
  <strong>{what} is not available yet.</strong> This daemon does not have that part built. Nothing was changed.
</div>
