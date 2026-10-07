<script lang="ts">
  import type { Decider, UrlName } from '../../lib/api/settings';
  import { patch, settings, setUrl, warningId as needed } from '../../lib/state/settings.svelte';
  import { composerProvider } from './keysInUse';

  /**
   * The decision model, its model name, the URL of a decider on the user's own endpoint, and
   * the daemon's warnings about what is still missing. Used by Settings and by the first-run
   * guide. `chosen` is the decider picked in the list, saved or not yet (one that needs a model
   * name is saved once it has one).
   */
  let { chosen = $bindable() }: { chosen?: Decider } = $props();

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
  const ollamaGuide = 'https://docs.ollama.com/quickstart';

  const s = $derived(settings.value);
  // Each follows the saved value; the control overrides it while it is being edited.
  let decider = $derived(s.decider);
  let deciderModel = $derived(s.decider_model);
  const picked = $derived(deciders.find((d) => d.id === decider));
  const shownUrls = $derived(urls.filter((u) => u.decider === decider || u.decider === composerProvider(s.composer_model)));
  let urlErrors = $state<Record<UrlName, string>>({ openai_base_url: '', ollama_url: '' });

  $effect(() => {
    chosen = decider;
  });

  // The decider and its model are validated together, so they are sent together. A decider
  // that needs a model waits for one; a refused change snaps back to what is saved.
  async function saveDecider() {
    if (picked?.needsModel && !deciderModel.trim()) return;
    if (!(await patch({ decider, decider_model: deciderModel.trim() }))) [decider, deciderModel] = [s.decider, s.decider_model];
  }
</script>

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
    {picked?.note}
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
    placeholder={picked?.needsModel ? 'Required' : "Provider's default"}
    aria-invalid={picked?.needsModel && !deciderModel.trim()}
    bind:value={deciderModel}
    onchange={saveDecider}
  />
  {#if picked?.needsModel && !deciderModel.trim()}
    <div role="alert" class="text-[12.5px] text-trash">{picked.name} has no default model. Name one to switch to it; until then MailRules keeps using the saved decision model.</div>
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
