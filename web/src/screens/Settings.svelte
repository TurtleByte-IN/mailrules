<script lang="ts">
  import type { Decider, KeyName } from '../lib/api/settings';
  import { confidence } from '../lib/format';
  import { patch, setKey, settings, toggleDryRun } from '../lib/state/settings.svelte';

  // Digest and Notification channels (P2) are added here with their backend, behind
  // features.digest and features.notifications.

  const deciders: { id: Decider; name: string; note: string }[] = [
    { id: 'jev', name: 'Jev (recommended)', note: 'Decision model with calibrated confidence. About $0.32 per month at 100 emails a day.' },
    { id: 'clef', name: 'Clef', note: 'Jev-compatible decision model from Cloudflare. Uses your Cloudflare account.' },
    { id: 'anthropic', name: 'Claude Haiku 4.5 only', note: 'Every uncertain email goes to Claude Haiku 4.5. About $1.25 to $2.35 per month at 100 emails a day.' },
    { id: 'ollama', name: 'Ollama on my server', note: 'Runs on your own server. Free, private, slower on small machines.' },
  ];
  const keys: { id: KeyName; label: string; placeholder: string }[] = [
    { id: 'openrouter', label: 'OpenRouter API key', placeholder: 'sk-or-v1-…' },
    { id: 'anthropic', label: 'Anthropic API key', placeholder: 'sk-ant-…' },
  ];

  const s = $derived(settings.value);
  // Follows the saved threshold; the slider overrides it while it is being dragged.
  let escalate = $derived(Math.round(s.escalateBelow * 100));
  // Write-only: a draft lives here until it is sent, then it is cleared.
  let drafts = $state<Record<KeyName, string>>({ openrouter: '', anthropic: '' });

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

  <section class="card flex flex-col gap-[18px] p-5">
    <div class="flex flex-col gap-1.5">
      <label for="set-decider" class="text-[13px] font-semibold">Decision model</label>
      <select
        id="set-decider"
        class="field h-11 max-w-[360px] px-2.5"
        value={s.decider}
        onchange={(e) => patch({ decider: e.currentTarget.value as Decider })}
      >
        {#each deciders as d (d.id)}
          <option value={d.id}>{d.name}</option>
        {/each}
      </select>
      <div class="text-[12.5px] text-secondary">{deciders.find((d) => d.id === s.decider)?.note}</div>
    </div>
    <label class="flex cursor-pointer items-center gap-2.5">
      <input type="checkbox" checked={s.fallback} onchange={() => patch({ fallback: !s.fallback })} />
      <span><span class="font-semibold">Ask Claude Haiku when unsure</span><span class="text-secondary"> · second opinion below the threshold</span></span>
    </label>
    <div class="flex max-w-[420px] flex-col gap-1.5">
      <label for="set-esc" class="text-[13px] font-semibold">Unsure below {confidence(escalate / 100)}</label>
      <input
        id="set-esc"
        type="range"
        min="50"
        max="95"
        class="h-10 accent-ink"
        value={escalate}
        oninput={(e) => (escalate = Number(e.currentTarget.value))}
        onchange={() => patch({ escalateBelow: escalate / 100 })}
      />
      <div class="text-[12.5px] text-secondary">Below this, MailRules asks the fallback; if still unsure, the email goes to Needs review.</div>
    </div>
    <div class="flex flex-col gap-1.5">
      <label for="set-ret" class="text-[13px] font-semibold">Keep email snippets for</label>
      <select
        id="set-ret"
        class="field h-11 max-w-[220px] px-2.5"
        value={s.retentionDays}
        onchange={(e) => patch({ retentionDays: Number(e.currentTarget.value) })}
      >
        {#each [7, 30, 90] as days (days)}
          <option value={days}>{days} days</option>
        {/each}
      </select>
      <div class="text-[12.5px] text-secondary">Full email bodies are never stored.</div>
    </div>
    <label class="flex cursor-pointer items-center gap-2.5">
      <input type="checkbox" checked={s.dryRun} onchange={toggleDryRun} />
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
      </form>
    {/each}
  </section>

  {#if s.install}
    <section aria-label="Self-hosting" class="card p-5">
      <h2>Self-hosting</h2>
      <p class="mt-1 text-[13.5px] text-secondary">Version {s.install.version} · data in {s.install.dataDir} · listening on {s.install.listen}</p>
    </section>
  {/if}
</div>
