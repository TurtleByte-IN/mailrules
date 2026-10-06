<script lang="ts">
  import type { Decider, KeyName } from '../lib/api/settings';
  import { confidence } from '../lib/format';
  import { load, patch, setKey, settings, toggleDryRun } from '../lib/state/settings.svelte';

  // Digest and Notification channels (P2) are added here with their backend, behind
  // features.digest and features.notifications.

  // needsModel: the daemon refuses these deciders without a model name (they have no default).
  const deciders: { id: Decider; name: string; note: string; needsModel?: true }[] = [
    { id: 'jev', name: 'Jev (recommended)', note: 'Decision model with calibrated confidence. About $0.32 per month at 100 emails a day.' },
    { id: 'clef', name: 'Clef', note: 'Jev-compatible decision model from Cloudflare. Uses your Cloudflare account.' },
    { id: 'anthropic', name: 'Claude Haiku 4.5 only', note: 'Every uncertain email goes to Claude Haiku 4.5. About $1.25 to $2.35 per month at 100 emails a day.' },
    { id: 'openai', name: 'OpenAI-compatible endpoint', note: 'Uses your OpenAI API key. Name the model below.', needsModel: true },
    { id: 'ollama', name: 'Ollama on my server', note: 'Runs on your own server. Free, private, slower on small machines. Name the model below.', needsModel: true },
  ];
  const keys: { id: KeyName; label: string; placeholder: string }[] = [
    { id: 'openrouter_api_key', label: 'OpenRouter API key', placeholder: 'sk-or-v1-…' },
    { id: 'cloudflare_account_id', label: 'Cloudflare account ID', placeholder: '' },
    { id: 'cloudflare_api_token', label: 'Cloudflare API token', placeholder: '' },
    { id: 'anthropic_api_key', label: 'Anthropic API key', placeholder: 'sk-ant-…' },
    { id: 'openai_api_key', label: 'OpenAI API key', placeholder: 'sk-…' },
  ];

  const s = $derived(settings.value);
  // Each follows the saved value; the control overrides it while it is being edited.
  let decider = $derived(s.decider);
  let deciderModel = $derived(s.decider_model);
  let escalate = $derived(Math.round(s.escalate_below * 100));
  let act = $derived(Math.round(s.min_confidence * 100));
  const chosen = $derived(deciders.find((d) => d.id === decider));
  // Write-only: a draft lives here until it is sent, then it is cleared.
  let drafts = $state<Record<KeyName, string>>({ openrouter_api_key: '', cloudflare_account_id: '', cloudflare_api_token: '', anthropic_api_key: '', openai_api_key: '' });

  // The decider and its model are validated together, so they are sent together. A decider
  // that needs a model waits for one; a refused change snaps back to what is saved.
  async function saveDecider() {
    if (chosen?.needsModel && !deciderModel.trim()) return;
    if (!(await patch({ decider, decider_model: deciderModel.trim() }))) [decider, deciderModel] = [s.decider, s.decider_model];
  }

  /** Saves one text or number field; a refused value snaps back to the saved one. */
  async function saveField(e: Event & { currentTarget: HTMLInputElement }, name: 'fallback_model' | 'composer_model' | 'retention_days') {
    const el = e.currentTarget;
    if (!(await patch({ [name]: name === 'retention_days' ? el.valueAsNumber : el.value.trim() }))) el.value = String(s[name]);
  }

  function saveKey(e: SubmitEvent, id: KeyName, label: string) {
    e.preventDefault();
    const secret = drafts[id].trim();
    drafts[id] = '';
    if (secret) setKey(id, secret, label);
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
      <div class="flex flex-col gap-1.5">
        <label for="set-decider" class="text-[13px] font-semibold">Decision model</label>
        <select
          id="set-decider"
          class="field h-11 max-w-[360px] px-2.5"
          bind:value={decider}
          onchange={() => {
            deciderModel = '';
            saveDecider();
          }}
        >
          {#each deciders as d (d.id)}
            <option value={d.id}>{d.name}</option>
          {/each}
        </select>
        <div class="text-[12.5px] text-secondary">{chosen?.note}</div>
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="set-decider-model" class="text-[13px] font-semibold">Decision model name</label>
        <input
          id="set-decider-model"
          class="field h-11 max-w-[360px] font-mono text-[13px]"
          autocomplete="off"
          placeholder={chosen?.needsModel ? 'Required' : "Provider's default"}
          aria-invalid={chosen?.needsModel && !deciderModel.trim()}
          bind:value={deciderModel}
          onchange={saveDecider}
        />
        {#if chosen?.needsModel && !deciderModel.trim()}
          <div role="alert" class="text-[12.5px] text-trash">{chosen.name} has no default model. Name one to switch to it; until then MailRules keeps using the saved decision model.</div>
        {:else}
          <div class="text-[12.5px] text-secondary">Leave empty to use the provider's default.</div>
        {/if}
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="set-fallback" class="text-[13px] font-semibold">Fallback model</label>
        <input id="set-fallback" class="field h-11 max-w-[360px] font-mono text-[13px]" autocomplete="off" value={s.fallback_model} onchange={(e) => saveField(e, 'fallback_model')} />
        <div class="text-[12.5px] text-secondary">Second opinion when the decision model is unsure. Leave empty to turn the fallback off.</div>
      </div>
      <div class="flex flex-col gap-1.5">
        <label for="set-composer" class="text-[13px] font-semibold">Rule composer model</label>
        <input id="set-composer" class="field h-11 max-w-[360px] font-mono text-[13px]" autocomplete="off" value={s.composer_model} onchange={(e) => saveField(e, 'composer_model')} />
        <div class="text-[12.5px] text-secondary">Turns what you describe into rules.</div>
      </div>
      <div class="flex max-w-[420px] flex-col gap-1.5">
        <label for="set-esc" class="text-[13px] font-semibold">Unsure below {confidence(escalate / 100)}</label>
        <input
          id="set-esc"
          type="range"
          min="0"
          max="100"
          class="h-10 accent-ink"
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
          class="h-10 accent-ink"
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
    </section>

    <section aria-label="Model API keys" class="card flex flex-col gap-3.5 p-5">
      <h2>Model API keys</h2>
      <p class="text-[13.5px] text-secondary">Keys are encrypted with your master key and never logged.</p>
      {#each keys as k (k.id)}
        <form class="flex flex-wrap items-end gap-x-3 gap-y-2" onsubmit={(e) => saveKey(e, k.id, k.label)}>
          <label class="flex flex-[1_1_260px] flex-col gap-1.5">
            <span class="flex items-center gap-2 text-[13px] font-semibold">
              {k.label}
              <span class="chip {s.keys[k.id] ? '' : 'chip-neutral'}">{s.keys[k.id] ? 'Set' : 'Not set'}</span>
            </span>
            <input class="field font-mono text-[13px]" type="password" autocomplete="off" placeholder={k.placeholder} bind:value={drafts[k.id]} />
          </label>
          <button class="btn font-semibold" disabled={!drafts[k.id].trim()}>{s.keys[k.id] ? 'Replace' : 'Save'}</button>
          {#if s.keys[k.id]}
            <button type="button" class="btn text-trash" aria-label="Remove {k.label}" onclick={() => setKey(k.id, '', k.label)}>Remove</button>
          {/if}
        </form>
      {/each}
    </section>

    {#if s.server.mode === 'selfhost'}
      <section aria-label="Self-hosting" class="card p-5">
        <h2>Self-hosting</h2>
        <p class="mt-1 text-[13.5px] text-secondary">Version {s.server.version} · data in {s.server.data_dir} · listening on {s.server.listen}</p>
      </section>
    {/if}
  {/if}
</div>
