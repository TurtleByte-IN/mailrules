<script lang="ts">
  import { limitRefused, type TestProgress } from '../api/rules';
  import { settings } from '../state/settings.svelte';
  import { limitProblem, testLimit } from '../state/testlimit.svelte';
  import Waiting from './Waiting.svelte';

  /**
   * Starts a rule test on `limit` emails and reports how far it is. `run` makes the request (the test
   * endpoint) and shows its result wherever the screen shows it; it passes `onProgress` on. A limit the
   * daemon refuses is thrown back here and shown beside the number; `run` deals with every other error.
   * The pieces are laid out as children of the screen's row of buttons.
   */
  let { run, tall = false, buttonClass = 'btn font-semibold' }: { run: (limit: number, onProgress: (p: TestProgress) => void) => Promise<void>; tall?: boolean; buttonClass?: string } = $props();

  let box: HTMLInputElement;
  let running = $state(false);
  // What the running test was asked to read, and how far it has got; null until the mail is listed.
  let asked = $state(0);
  let progress = $state<TestProgress | null>(null);
  // The daemon's sentence when it refused the number; goes when the number changes.
  let refused = $state('');

  const problem = $derived(refused || limitProblem(testLimit.value));
  const noun = (n: number) => (n === 1 ? 'email' : 'emails');
  const label = $derived(problem ? 'Test' : `Test on last ${testLimit.value} ${noun(testLimit.value!)}`);
  const pct = $derived(progress?.total ? Math.round((progress.done / progress.total) * 100) : 0);

  async function start() {
    if (limitProblem(testLimit.value)) return box.focus();
    asked = testLimit.value!;
    running = true;
    progress = null;
    refused = '';
    try {
      await run(asked, (p) => (progress = p));
    } catch (e) {
      if (!limitRefused(e)) throw e;
      refused = e.message;
      box.focus();
    } finally {
      running = false;
    }
  }
</script>

<input
  bind:this={box}
  type="number"
  min="1"
  max={settings.value.limits.test_max || undefined}
  step="1"
  inputmode="numeric"
  aria-label="How many emails to test"
  aria-invalid={problem ? true : undefined}
  aria-describedby={problem ? 'test-limit-problem' : undefined}
  class="field {tall ? 'h-11' : 'h-10'} w-24 px-2.5 font-mono"
  disabled={running}
  bind:value={testLimit.value}
  oninput={() => (refused = '')}
/>
<button type="button" class={buttonClass} disabled={running} onclick={start}>{running ? `Testing on last ${asked} ${noun(asked)}…` : label}</button>
{#if problem}
  <p id="test-limit-problem" role="alert" class="w-full text-[12.5px] text-trash">{problem}</p>
{/if}
{#if running}
  <div class="flex w-full flex-col gap-1.5">
    {#if progress}
      <div role="progressbar" aria-label="Test progress" aria-valuemin={0} aria-valuemax={100} aria-valuenow={pct} class="h-3 overflow-hidden rounded bg-neutral">
        <div class="h-3 bg-ink" style:width="{pct}%"></div>
      </div>
      <div role="status" class="text-[13px] text-nav">
        {progress.done.toLocaleString()} of {progress.total.toLocaleString()} emails tested{progress.model_calls
          ? ` · ${progress.model_calls.toLocaleString()} sent to the decision model`
          : ''}
      </div>
    {:else}
      <Waiting text="Reading your mailbox" />
    {/if}
  </div>
{/if}
