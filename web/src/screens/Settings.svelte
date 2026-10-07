<script lang="ts">
  import { onMount } from 'svelte';
  import { anthropicWorkspaces, TRASH_FOLDER, type AnthropicWorkspaces, type Decider, type KeyName, type UrlName } from '../lib/api/settings';
  import { composerProvider, keysInUse } from './settings/keysInUse';
  import { confidence } from '../lib/format';
  import { rules } from '../lib/state/rules.svelte';
  import { load, patch, setKey, settings, setUrl, setWorkspace, toggleDryRun } from '../lib/state/settings.svelte';

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
  // Where a decider that runs on the user's own endpoint is reached; shown while the decider or
  // the rule composer model uses that provider.
  const urls: { id: UrlName; decider: Decider; label: string; placeholder: string }[] = [
    { id: 'openai_base_url', decider: 'openai', label: 'Endpoint URL', placeholder: 'api.openai.com' },
    { id: 'ollama_url', decider: 'ollama', label: 'Ollama server URL', placeholder: 'http://localhost:11434' },
  ];
  const keySource = { stored: 'Set', environment: 'Set by environment', none: 'Not set' };
  // Where to get each credential; fixed text, opened in a new tab. Both Cloudflare rows share one guide.
  const cloudflareGuide = 'https://developers.cloudflare.com/workers-ai/get-started/rest-api/';
  const keyGuides: Record<KeyName, string> = {
    openrouter_api_key: 'https://openrouter.ai/docs/api-reference/authentication',
    cloudflare_account_id: cloudflareGuide,
    cloudflare_api_token: cloudflareGuide,
    anthropic_api_key: 'https://platform.claude.com/docs/en/api/overview',
    openai_api_key: 'https://developers.openai.com/api/docs/quickstart',
  };
  const ollamaGuide = 'https://docs.ollama.com/quickstart';
  // Where the Claude Console lists a workspace's ID.
  const workspaceGuide = 'https://platform.claude.com/settings/workspaces';
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
  const inUse = $derived(keysInUse({ decider: s.decider, fallback_model: s.fallback_model, composer_model: s.composer_model, keys: s.keys, ruleModels: rules.list.map((r) => r.model) }));
  const shownUrls = $derived(urls.filter((u) => u.decider === decider || u.decider === composerProvider(s.composer_model)));
  /** The id of the warning that names this setting (as SettingsPatch spells it), when there is one. */
  const needed = (path: string) => (s.warnings.some((w) => w.path === path) ? 'warn-' + path : undefined);
  let urlErrors = $state<Record<UrlName, string>>({ openai_base_url: '', ollama_url: '' });
  // Write-only: a draft lives here until it is sent, then it is cleared.
  let drafts = $state<Record<KeyName, string>>({ openrouter_api_key: '', cloudflare_account_id: '', cloudflare_api_token: '', anthropic_api_key: '', openai_api_key: '' });

  // The Claude workspace (MAI-57). Only a Claude key that covers a whole organisation needs
  // one, so the control shows only once one is set, to pick from, to type, or warned about.
  let lookup = $state<AnthropicWorkspaces | null>(null);
  let workspaceDraft = $state('');
  let workspaceError = $state('');
  const claudeKey = $derived(s.keys.anthropic_api_key !== 'none');
  const showWorkspace = $derived(
    claudeKey && (!!s.anthropic_workspace_id || lookup?.status === 'several' || lookup?.status === 'failed' || !!needed('anthropic_workspace_id')),
  );

  /** Asks the daemon which workspace the Claude key needs, while one is in force and none is set. */
  async function findWorkspace() {
    lookup = null;
    if (!claudeKey || s.anthropic_workspace_id) return;
    try {
      lookup = await anthropicWorkspaces();
    } catch {
      // The control stays hidden; the field is still reachable through a warning.
    }
  }
  // The shell loaded the settings once; a warning may have appeared since (a Claude request
  // refused for want of a workspace), so the screen reads them afresh when it opens. A failed
  // load is left to its Retry.
  onMount(async () => {
    if (!settings.loaded) return;
    await load();
    await findWorkspace();
  });

  async function saveWorkspace(id: string | null) {
    workspaceError = await setWorkspace(id);
    if (!workspaceError) workspaceDraft = '';
  }

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

  /** A refused change unticks or reticks the box to what is saved. */
  async function saveTrashToFolder(e: Event & { currentTarget: HTMLInputElement }) {
    const el = e.currentTarget;
    if (!(await patch({ trash_to_folder: el.checked }))) el.checked = s.trash_to_folder;
  }

  async function saveKey(e: SubmitEvent, id: KeyName, label: string) {
    e.preventDefault();
    const secret = drafts[id].trim();
    drafts[id] = '';
    if (!secret) return;
    await setKey(id, secret, label);
    if (id === 'anthropic_api_key') findWorkspace();
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
        <div class="text-[12.5px] text-secondary">
          {chosen?.note}
          {#if decider === 'ollama'}
            <a href={ollamaGuide} target="_blank" rel="noopener noreferrer" class="font-semibold">Setup guide</a>
          {/if}
        </div>
        {#each s.warnings as w (w.path)}
          <div id="warn-{w.path}" role="status" class="rounded-md border border-warn-line bg-warn-bg px-4 py-3 text-warn">{w.message}</div>
        {/each}
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
      {#each shownUrls as u (u.id)}
        <div class="flex flex-col gap-1.5">
          <label for="set-{u.id}" class="text-[13px] font-semibold">{u.label}</label>
          <input
            id="set-{u.id}"
            class="field h-11 max-w-[360px] font-mono text-[13px]"
            type="url"
            autocomplete="off"
            placeholder={u.placeholder}
            aria-invalid={!!urlErrors[u.id]}
            aria-describedby={needed(u.id)}
            value={s[u.id]}
            onchange={async (e) => {
              const el = e.currentTarget;
              urlErrors[u.id] = await setUrl(u.id, el.value.trim());
            }}
          />
          {#if urlErrors[u.id]}
            <div role="alert" class="text-[12.5px] text-trash">{urlErrors[u.id]}</div>
          {/if}
          {#if needed(u.id)}
            <div class="text-[12.5px] text-warn">Needed</div>
          {/if}
        </div>
      {/each}
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
      <label class="flex cursor-pointer items-center gap-2.5">
        <input type="checkbox" checked={s.trash_to_folder} onchange={saveTrashToFolder} />
        <span><span class="font-semibold">Send trashed mail to {TRASH_FOLDER}</span><span class="text-secondary"> · providers empty Trash on their own; MailRules' folder is never emptied, so mail trashed by mistake can still be found</span></span>
      </label>
    </section>

    {#snippet workspaceRow()}
      <div class="flex flex-col gap-1.5 border-l-2 border-line-divider pl-3.5">
        <span class="flex items-center gap-2 text-[13px] font-semibold">
          <label for="set-workspace">Anthropic workspace</label>
          {#if s.anthropic_workspace_found}
            <span class="chip">Found automatically</span>
          {/if}
          {#if needed('anthropic_workspace_id')}
            <span class="text-[12.5px] font-normal text-warn">Needed</span>
          {/if}
          <a href={workspaceGuide} target="_blank" rel="noopener noreferrer" class="ml-auto text-[12.5px] font-semibold">How to find it</a>
        </span>
        <div class="text-[12.5px] text-secondary">Your Claude key covers your whole organisation, so every request to Claude names this workspace.</div>
        {#if s.anthropic_workspace_id}
          <div class="text-[13.5px]">
            {#if s.anthropic_workspace_name}<span class="font-semibold">{s.anthropic_workspace_name}</span>{' · '}{/if}<code class="font-mono text-[13px]">{s.anthropic_workspace_id}</code>
          </div>
        {/if}
        {#if lookup?.status === 'several'}
          <select
            aria-label="Choose a workspace"
            class="field h-11 max-w-[360px] px-2.5"
            value={s.anthropic_workspace_id}
            onchange={(e) => saveWorkspace(e.currentTarget.value)}
          >
            <option value="" disabled>Choose a workspace</option>
            {#each lookup.workspaces as w (w.id)}
              <option value={w.id}>{w.name} ({w.id})</option>
            {/each}
          </select>
        {:else if lookup?.status === 'failed' && !s.anthropic_workspace_id}
          <div class="text-[12.5px] text-warn">MailRules could not look up your workspaces. Type the workspace ID.</div>
        {/if}
        <form
          class="flex flex-wrap items-center gap-2"
          onsubmit={(e) => {
            e.preventDefault();
            saveWorkspace(workspaceDraft.trim());
          }}
        >
          <input
            id="set-workspace"
            class="field flex-[1_1_260px] font-mono text-[13px]"
            autocomplete="off"
            placeholder="wrkspc_…"
            aria-invalid={!!workspaceError}
            aria-describedby={needed('anthropic_workspace_id')}
            bind:value={workspaceDraft}
          />
          <button class="btn font-semibold" aria-label="Save Anthropic workspace" disabled={!workspaceDraft.trim()}>Save</button>
          {#if s.anthropic_workspace_id}
            <button type="button" class="btn text-trash" aria-label="Remove Anthropic workspace" onclick={() => saveWorkspace(null)}>Remove</button>
          {/if}
        </form>
        {#if workspaceError}
          <div role="alert" class="text-[12.5px] text-trash">{workspaceError}</div>
        {/if}
      </div>
    {/snippet}

    {#snippet keyRow(k: (typeof keys)[number])}
      <form class="flex flex-wrap items-end gap-x-3 gap-y-2" onsubmit={(e) => saveKey(e, k.id, k.label)}>
        <label class="flex flex-[1_1_260px] flex-col gap-1.5">
          <span class="flex items-center gap-2 text-[13px] font-semibold">
            {k.label}
            <span class="chip {s.keys[k.id] === 'none' ? 'chip-neutral' : ''}">{keySource[s.keys[k.id]]}</span>
            {#if needed('keys.' + k.id)}
              <span class="text-[12.5px] font-normal text-warn">Needed</span>
            {/if}
            <a href={keyGuides[k.id]} target="_blank" rel="noopener noreferrer" class="ml-auto text-[12.5px] font-semibold">How to get this</a>
          </span>
          <input class="field font-mono text-[13px]" type="password" autocomplete="off" placeholder={k.placeholder} aria-describedby={needed('keys.' + k.id)} bind:value={drafts[k.id]} />
        </label>
        <button class="btn font-semibold" disabled={!drafts[k.id].trim()}>{s.keys[k.id] === 'stored' ? 'Replace' : 'Save'}</button>
        {#if s.keys[k.id] === 'stored'}
          <button
            type="button"
            class="btn text-trash"
            aria-label="Remove {k.label}"
            onclick={async () => {
              await setKey(k.id, '', k.label);
              if (k.id === 'anthropic_api_key') findWorkspace();
            }}>Remove</button
          >
        {/if}
      </form>
      {#if k.id === 'anthropic_api_key' && showWorkspace}
        {@render workspaceRow()}
      {/if}
    {/snippet}

    <section aria-label="Model API keys" class="card flex flex-col gap-3.5 p-5">
      <h2>Model API keys</h2>
      <p class="text-[13.5px] text-secondary">Keys are encrypted with your master key and never logged.</p>
      {#each keys.filter((k) => inUse.has(k.id)) as k (k.id)}
        {@render keyRow(k)}
      {/each}
      {#if keys.some((k) => !inUse.has(k.id))}
        <details class="border-t border-line-divider pt-3.5">
          <summary class="min-h-8 cursor-pointer font-semibold">Other providers</summary>
          <div class="flex flex-col gap-3.5 pt-3">
            {#each keys.filter((k) => !inUse.has(k.id)) as k (k.id)}
              {@render keyRow(k)}
            {/each}
          </div>
        </details>
      {/if}
    </section>

    {#if s.server.mode === 'selfhost'}
      <section aria-label="Self-hosting" class="card p-5">
        <h2>Self-hosting</h2>
        <p class="mt-1 text-[13.5px] text-secondary">Version {s.server.version} · data in {s.server.data_dir} · listening on {s.server.listen}</p>
      </section>
    {/if}
  {/if}
</div>
