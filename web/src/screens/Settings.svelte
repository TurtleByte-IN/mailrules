<script lang="ts">
  import { onMount } from 'svelte';
  import { TRASH_FOLDER } from '../lib/api/settings';
  import DeciderFields from './settings/DeciderFields.svelte';
  import KeyFields from './settings/KeyFields.svelte';
  import { keyOrder, keysInUse } from './settings/keysInUse';
  import { confidence } from '../lib/format';
  import { rules } from '../lib/state/rules.svelte';
  import { findWorkspace, load, patch, settings, toggleDryRun } from '../lib/state/settings.svelte';
  import { choose, stored, type Choice } from '../lib/theme';

  // Digest and Notification channels (P2) are added here with their backend, behind
  // features.digest and features.notifications.

  const s = $derived(settings.value);
  // Each follows the saved value; the control overrides it while it is being edited.
  let escalate = $derived(Math.round(s.escalate_below * 100));
  let act = $derived(Math.round(s.min_confidence * 100));
  const inUse = $derived(keysInUse({ decider: s.decider, fallback_model: s.fallback_model, composer_model: s.composer_model, keys: s.keys, ruleModels: rules.list.map((r) => r.model) }));

  // The shell loaded the settings once; a warning may have appeared since (a Claude request
  // refused for want of a workspace), so the screen reads them afresh when it opens. A failed
  // load is left to its Retry.
  onMount(async () => {
    if (!settings.loaded) return;
    await load();
    await findWorkspace();
  });

  /** Saves one text or number field; a refused value snaps back to the saved one. */
  async function saveField(e: Event & { currentTarget: HTMLInputElement }, name: 'fallback_model' | 'composer_model' | 'retention_days') {
    const el = e.currentTarget;
    if (!(await patch({ [name]: name === 'retention_days' ? el.valueAsNumber : el.value.trim() }))) el.value = String(s[name]);
  }

  /** Saves one switch; a refused change unticks or reticks the box to what is saved. */
  async function saveSwitch(e: Event & { currentTarget: HTMLInputElement }, name: 'trash_to_folder' | 'leave_own_mail') {
    const el = e.currentTarget;
    if (!(await patch({ [name]: el.checked }))) el.checked = s[name];
  }
</script>

<div class="flex max-w-[760px] flex-col gap-[18px]">
  <header>
    <h1>Settings</h1>
    <p class="mt-1 text-secondary">How MailRules decides, and how careful it is.</p>
  </header>

  {#if settings.error}
    <div role="alert" class="flex flex-wrap items-center justify-between gap-2 rounded bg-trash-bg px-[18px] py-3 text-trash">
      <span>Could not load the settings. {settings.error}</span>
      <button type="button" class="btn" onclick={load}>Retry</button>
    </div>
  {/if}

  {#if settings.loaded}
    <section class="card flex flex-col gap-[18px] p-5">
      <DeciderFields />
      <div class="flex flex-col gap-1.5">
        <label for="set-fallback" class="text-[13px] font-semibold">Fallback model</label>
        <input id="set-fallback" class="field h-11 max-w-[360px] font-mono text-[13px]" autocomplete="off" value={s.fallback_model} onchange={(e) => saveField(e, 'fallback_model')} />
        <div class="text-[12.5px] text-secondary">Second opinion when the decision model is unsure. Leave empty to turn the fallback off.</div>
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="set-composer" class="text-[13px] font-semibold">Rule composer model</label>
        <input id="set-composer" class="field h-11 max-w-[360px] font-mono text-[13px]" autocomplete="off" aria-describedby="set-composer-forms" value={s.composer_model} onchange={(e) => saveField(e, 'composer_model')} />
        <div class="text-[12.5px] text-secondary">Turns what you describe into rules.</div>
        <div id="set-composer-forms" class="text-[12.5px] text-secondary">A Claude model such as <code class="font-mono">claude-haiku-4-5</code>, or <code class="font-mono">openai:gpt-4o-mini</code> or <code class="font-mono">ollama:llama3.2</code>.</div>
      </div>
      <div class="flex max-w-[420px] flex-col gap-1.5">
        <label for="set-esc" class="text-[13px] font-semibold">Unsure below {confidence(escalate / 100)}</label>
        <input
          id="set-esc"
          type="range"
          min="0"
          max="100"
          class="h-10 accent-ink max-md:h-11"
          value={escalate}
          oninput={(e) => (escalate = Number(e.currentTarget.value))}
          onchange={async () => {
            if (!(await patch({ escalate_below: escalate / 100 }))) escalate = Math.round(s.escalate_below * 100);
          }}
        />
        <div class="text-[12.5px] text-secondary">Below this, MailRules asks the fallback; if still unsure, the email goes to Needs review.</div>
      </div>
      <div class="flex max-w-[420px] flex-col gap-1.5">
        <label for="set-act" class="text-[13px] font-semibold">Act at {confidence(act / 100)} or above</label>
        <input
          id="set-act"
          type="range"
          min="0"
          max="100"
          class="h-10 accent-ink max-md:h-11"
          value={act}
          oninput={(e) => (act = Number(e.currentTarget.value))}
          onchange={async () => {
            if (!(await patch({ min_confidence: act / 100 }))) act = Math.round(s.min_confidence * 100);
          }}
        />
        <div class="text-[12.5px] text-secondary">The default for every rule; a rule can set its own.</div>
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="set-ret" class="text-[13px] font-semibold">Keep email snippets for</label>
        <div class="flex items-center gap-2">
          <input id="set-ret" class="field h-11 w-[110px] px-2.5" type="number" min="1" max="3650" value={s.retention_days} onchange={(e) => saveField(e, 'retention_days')} />
          <span>days</span>
        </div>
        <div class="text-[12.5px] text-secondary">1 to 3650 days. Full email bodies are never stored.</div>
      </div>
      <label class="flex cursor-pointer items-center gap-2.5">
        <input type="checkbox" checked={s.dry_run} onchange={toggleDryRun} />
        <span><span class="font-semibold">Dry-run</span><span class="text-secondary"> · log decisions without touching the mailbox</span></span>
      </label>
      <label class="flex cursor-pointer items-center gap-2.5">
        <input type="checkbox" checked={s.trash_to_folder} onchange={(e) => saveSwitch(e, 'trash_to_folder')} />
        <span><span class="font-semibold">Send trashed mail to {TRASH_FOLDER}</span><span class="text-secondary"> · providers empty Trash on their own; MailRules' folder is never emptied, so mail trashed by mistake can still be found</span></span>
      </label>
      <label class="flex cursor-pointer items-center gap-2.5">
        <input type="checkbox" checked={s.leave_own_mail} onchange={(e) => saveSwitch(e, 'leave_own_mail')} />
        <span><span class="font-semibold">Leave my own emails alone</span><span class="text-secondary"> · mail sent from this mailbox's own address is never sorted, trashed or sent to the AI</span></span>
      </label>
    </section>

    <section aria-label="Model API keys" class="card flex flex-col gap-3.5 p-5">
      <h2>Model API keys</h2>
      <p class="text-[13.5px] text-secondary">Keys are encrypted with your master key and never logged.</p>
      <KeyFields shown={keyOrder.filter((k) => inUse.has(k))} other={keyOrder.filter((k) => !inUse.has(k))} />
    </section>

    {#if s.server.mode === 'selfhost'}
      <section aria-label="Self-hosting" class="card p-5">
        <h2>Self-hosting</h2>
        <p class="mt-1 text-[13.5px] text-secondary">Version {s.server.version} · data in {s.server.data_dir} · listening on {s.server.listen}</p>
      </section>
    {/if}
  {/if}

  <section aria-label="Appearance" class="card flex flex-col gap-1.5 p-5">
    <label for="set-theme" class="text-[13px] font-semibold">Appearance</label>
    <select id="set-theme" class="field h-11 max-w-[360px] px-2.5" value={stored()} onchange={(e) => choose(e.currentTarget.value as Choice)}>
      <option value="system">System</option>
      <option value="light">Light</option>
      <option value="dark">Dark</option>
    </select>
    <div class="text-[12.5px] text-secondary">System follows your device's light or dark setting. Saved in this browser only.</div>
  </section>
</div>
