<script lang="ts">
  import type { KeyName } from '../../lib/api/settings';
  import { findWorkspace, setKey, settings, setWorkspace, warningId as needed, workspaces } from '../../lib/state/settings.svelte';

  /**
   * The provider key fields, write-only: `shown` up front in that order, `other` under a
   * collapsed "Other providers". Used by Settings and by the first-run guide.
   */
  let { shown, other = [] }: { shown: KeyName[]; other?: KeyName[] } = $props();

  const keySource = { stored: 'Set', environment: 'Set by environment', none: 'Not set' };
  // Where to get each credential; fixed text, opened in a new tab. Both Cloudflare rows share one guide.
  const cloudflareGuide = 'https://developers.cloudflare.com/workers-ai/get-started/rest-api/';
  const keys: Record<KeyName, { label: string; placeholder: string; guide: string }> = {
    openrouter_api_key: { label: 'OpenRouter API key', placeholder: 'sk-or-v1-…', guide: 'https://openrouter.ai/docs/api-reference/authentication' },
    cloudflare_account_id: { label: 'Cloudflare account ID', placeholder: '', guide: cloudflareGuide },
    cloudflare_api_token: { label: 'Cloudflare API token', placeholder: '', guide: cloudflareGuide },
    anthropic_api_key: { label: 'Anthropic API key', placeholder: 'sk-ant-…', guide: 'https://platform.claude.com/docs/en/api/overview' },
    openai_api_key: { label: 'OpenAI API key', placeholder: 'sk-…', guide: 'https://developers.openai.com/api/docs/quickstart' },
  };
  // Where the Claude Console lists a workspace's ID.
  const workspaceGuide = 'https://platform.claude.com/settings/workspaces';

  const s = $derived(settings.value);
  // Write-only: a draft lives here until it is sent, then it is cleared.
  let drafts = $state<Record<KeyName, string>>({ openrouter_api_key: '', cloudflare_account_id: '', cloudflare_api_token: '', anthropic_api_key: '', openai_api_key: '' });

  // The Claude workspace (MAI-57). Only a Claude key that covers a whole organisation needs
  // one, so the control shows only once one is set, to pick from, to type, or warned about.
  let workspaceDraft = $state('');
  let workspaceError = $state('');
  const lookup = $derived(workspaces.lookup);
  const showWorkspace = $derived(
    s.keys.anthropic_api_key !== 'none' &&
      (!!s.anthropic_workspace_id || lookup?.status === 'several' || lookup?.status === 'failed' || !!needed('anthropic_workspace_id')),
  );

  async function saveWorkspace(id: string | null) {
    workspaceError = await setWorkspace(id);
    if (!workspaceError) workspaceDraft = '';
  }

  async function saveKey(e: SubmitEvent, id: KeyName) {
    e.preventDefault();
    const secret = drafts[id].trim();
    drafts[id] = '';
    if (!secret) return;
    await setKey(id, secret, keys[id].label);
    if (id === 'anthropic_api_key') findWorkspace();
  }
</script>

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

{#snippet keyRow(id: KeyName)}
  {@const k = keys[id]}
  <form class="flex flex-wrap items-end gap-x-3 gap-y-2" onsubmit={(e) => saveKey(e, id)}>
    <label class="flex flex-[1_1_260px] flex-col gap-1.5">
      <span class="flex items-center gap-2 text-[13px] font-semibold max-md:flex-wrap">
        {k.label}
        <span class="chip {s.keys[id] === 'none' ? 'chip-neutral' : ''}">{keySource[s.keys[id]]}</span>
        {#if needed('keys.' + id)}
          <span class="text-[12.5px] font-normal text-warn">Needed</span>
        {/if}
        <a href={k.guide} target="_blank" rel="noopener noreferrer" class="ml-auto text-[12.5px] font-semibold">How to get this</a>
      </span>
      <input class="field font-mono text-[13px]" type="password" autocomplete="off" placeholder={k.placeholder} aria-describedby={needed('keys.' + id)} bind:value={drafts[id]} />
    </label>
    <button class="btn font-semibold" disabled={!drafts[id].trim()}>{s.keys[id] === 'stored' ? 'Replace' : 'Save'}</button>
    {#if s.keys[id] === 'stored'}
      <button
        type="button"
        class="btn text-trash"
        aria-label="Remove {k.label}"
        onclick={async () => {
          await setKey(id, '', k.label);
          if (id === 'anthropic_api_key') findWorkspace();
        }}>Remove</button
      >
    {/if}
  </form>
  {#if id === 'anthropic_api_key' && showWorkspace}
    {@render workspaceRow()}
  {/if}
{/snippet}

{#each shown as id (id)}
  {@render keyRow(id)}
{/each}
{#if other.length}
  <details class="border-t border-line-divider pt-3.5">
    <summary class="min-h-8 cursor-pointer font-semibold max-md:min-h-11">Other providers</summary>
    <div class="flex flex-col gap-3.5 pt-3">
      {#each other as id (id)}
        {@render keyRow(id)}
      {/each}
    </div>
  </details>
{/if}
