---
title: Decision models and keys
description: Choose the model that decides which rule an email matches, set its keys and thresholds, and see what it costs.
order: 50
---

MailRules uses AI models for three jobs:

- The **decision model** reads a new email and picks which of your plain-English rules it matches, with a confidence.
- The **fallback model**, Claude Haiku 4.5 by default, gives a second opinion when the decision model is unsure.
- The **rule composer** turns what you write under **Add rules** → **Describe it** into rules.

Rules made only of conditions, and sender rules, never use a model.

All of this is set in **Settings**, or with environment variables. See [Keys: browser or environment](#keys-browser-or-environment) for which wins.

## Choosing the decision model

| Decision model | Provider | Needs | Default model |
| --- | --- | --- | --- |
| Jev (recommended) | [OpenRouter](https://openrouter.ai) | `OPENROUTER_API_KEY` | `typesafe/jev-1.13` |
| Clef | Cloudflare Workers AI, on your Cloudflare account | `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` | `clef` (`clef-flash` also works) |
| Claude Haiku 4.5 only | Anthropic | `ANTHROPIC_API_KEY` | `claude-haiku-4-5` |
| OpenAI-compatible endpoint | OpenAI, or any server with the same API | `OPENAI_API_KEY`, and `OPENAI_BASE_URL` unless it is OpenAI itself | none: name one |
| Ollama on my server | Your own [Ollama](https://docs.ollama.com/quickstart) server | `OLLAMA_URL`, such as `http://localhost:11434` | none: name one |

In Settings, pick it under **Decision model**. To use another model from the same provider, enter it under **Decision model name**; leave it empty for the default. With the environment, set `MAILRULES_DECIDER` to `jev`, `clef`, `anthropic`, `openai` or `ollama`, and `MAILRULES_DECIDER_MODEL` for the model.

Jev and Clef are decision models built for this kind of choice: they return a probability for every rule they were offered, and MailRules uses the probability of the chosen rule as its confidence. Claude, OpenAI-compatible models and Ollama models are general models that are asked to answer in a fixed format.

A missing key is shown next to the choice: "The … decision model needs …. Until it is set, rules that need a model are passed over." Until then, condition-only rules and sender rules keep working, and an email that only a plain-English rule could take waits in **Needs review**.

A rule can name its own decision model (see [Managing rules](./rules.md#managing-rules)). The key for that model must be set too.

## Thresholds and the fallback model

Two thresholds in Settings control how careful MailRules is:

- **Unsure below** (`MAILRULES_ESCALATE_BELOW`, default 75%). When the decision model's confidence is below this, the fallback model is asked as well, and its answer is used.
- **Act at … or above** (`MAILRULES_MIN_CONFIDENCE`, default 75%). A rule acts only when the final confidence reaches this. Below it, nothing is done and the email waits in **Needs review**. Each rule can set its own threshold.

The fallback model is set under **Fallback model** (`MAILRULES_FALLBACK_MODEL`, default `claude-haiku-4-5`). It is always a Claude model and uses the Anthropic API key. Leave the field empty to turn the fallback off.

The fallback is used only when an Anthropic API key is set. Without one, MailRules does not ask a second opinion, so unsure emails are acted on, or held in Needs review, on the decision model's answer alone. It says so in two places: the daemon logs one warning when it starts (and again whenever a setting changes it), `fallback model is not active, so low-confidence decisions are not double-checked`, and Settings shows `Not active: …` under **Fallback model**, with the reason. The model name stays in the field, so adding a key later turns the fallback on without retyping it. The same note appears when the decision model already is the fallback model (for example `anthropic` with `claude-haiku-4-5`), since there is no second opinion to ask for.

The fallback model also sees up to five of your past corrections that look most like the email (same sender, same domain or same mailing list first). The decision model does not.

## What is sent to a model

For each email that needs a model, MailRules sends:

- the sender's name and address, whether you have written to them before, and the recipient addresses;
- the subject;
- whether it is bulk mail, its List-Id, its DMARC result and the file extensions of its attachments (never the attachments themselves);
- the first 2000 characters of its plain text (`MAILRULES_BODY_CHARS`);
- the name and description of each candidate rule.

It is sent only to the provider of the model being asked. Nothing is sent for emails that conditions or sender rules decide, or for mail sent from your own address while **Leave my own emails alone** is on. Ollama keeps everything on your own server.

## The rule composer

The rule composer writes rules from your words, and rewrites a rule with **Rewrite with AI**. Set its model under **Rule composer model** (`MAILRULES_COMPOSER_MODEL`, default `claude-haiku-4-5`):

- a Claude model name, such as `claude-haiku-4-5`, uses the Anthropic API key;
- `openai:<model>`, such as `openai:gpt-4o-mini`, uses the OpenAI-compatible endpoint and key;
- `ollama:<model>`, such as `ollama:llama3.2`, uses your Ollama server.

Without a working composer model, **Describe it** and **Rewrite with AI** do not work. **Build with conditions** and **Templates** still do.

## Keys: browser or environment

Each provider setting can be set in the browser or in the environment:

| Setting in the browser | Environment variable |
| --- | --- |
| OpenRouter API key | `OPENROUTER_API_KEY` |
| Cloudflare account ID | `CLOUDFLARE_ACCOUNT_ID` |
| Cloudflare API token | `CLOUDFLARE_API_TOKEN` |
| Anthropic API key | `ANTHROPIC_API_KEY` |
| Anthropic workspace | `ANTHROPIC_WORKSPACE_ID` |
| OpenAI API key | `OPENAI_API_KEY` |
| Endpoint URL | `OPENAI_BASE_URL` |
| Ollama server URL | `OLLAMA_URL` |

In the browser, keys are entered under **Model API keys** in Settings. The keys in use are shown first; the rest are under **Other providers**. Each key shows **Set**, **Set by environment** or **Not set**, and has **Save** (or **Replace**) and **Remove**.

- What is saved in the browser wins over the environment. **Remove** forgets the saved value, and the environment variable, if any, applies again.
- Keys saved in the browser are encrypted with the master key and stored in the database. They are never shown again, never returned by the API and never logged.
- Keys in the environment are never written to the database.
- Changes made in the browser apply from the next email, without a restart. Changes to environment variables need a restart.

The same rule applies to the other settings the browser can change: the decision model and its name, the fallback and composer models, and both thresholds. `MAILRULES_BODY_CHARS`, `MAILRULES_MODEL_CONCURRENCY` and `MAILRULES_PRICES_FILE` can be set only in the environment. See the [settings reference](./settings.md).

### Anthropic workspaces

A Claude key made for one workspace needs nothing more. A key made for your whole organisation must name the workspace each request runs in. MailRules looks it up when you save the key: with one workspace it uses it ("Found automatically"); with several, Settings lists them under the key to choose from. If your key is not allowed to list workspaces, copy the ID (`wrkspc_…`) from the ID column of [Settings → Workspaces](https://platform.claude.com/settings/workspaces) in the Claude Console into **Anthropic workspace**, or set `ANTHROPIC_WORKSPACE_ID`.

## What it costs

The UI estimates about $0.32 a month at 100 emails a day for Jev, and $1.25 to $2.35 a month for Claude Haiku 4.5 alone. Ollama costs nothing. Your bill depends on your mail and comes from your provider; MailRules has no account or bill of its own.

**Usage** in the sidebar shows this month's numbers:

- **Emails acted on**, and how many were **Decided for free** by conditions and sender rules;
- **Model calls**, split between the decision model and the fallback;
- **Cost this month**, and **Model calls per day**;
- **By rule, this month** and **By model, this month**.

The cost includes calls made by rule tests, Cleanup and the rule composer. Overview and Activity show today's cost, and the [summary email](./summary-email.md) includes it too.

Costs are estimated from a built-in price table, in US dollars per million tokens. Jev's cost is the one OpenRouter reports when it reports one. OpenAI-compatible models have no built-in price and count as free until you add one. To add or change prices, point `MAILRULES_PRICES_FILE` at a JSON file; its entries are laid over the built-in table:

```json
{
  "openai": {
    "gpt-4o-mini": {"input": 0.15, "output": 0.6}
  }
}
```

The top-level keys are providers (`openrouter`, `cloudflare`, `anthropic`, `openai`, `ollama`), and each model has `input`, `output`, `cache_read` and `cache_write` prices. Use the model name as you set it; Clef models are written in full, such as `@cf/cloudflare/clef`. The file is read at startup; restart MailRules after changing it. A file that cannot be read stops MailRules from starting.

## Limits

- At most 8 model calls are in flight at once (`MAILRULES_MODEL_CONCURRENCY`).
- Each call times out after 15 seconds. A call refused for rate limiting or a server error is retried twice.
- If a model's answer cannot be read after three tries, the email is left where it is.
- If the model cannot be reached, the email is tried again every 5 minutes, six times, and then waits in Needs review.

## Comparing models on your own mail

`mailrules eval` measures how often each decision model picks the right rule, from a file of labelled emails you write yourself. It reads keys from the environment only, sends every email to the models (which costs money), and writes nothing to the database.

```bash
mailrules eval --labels labeled.jsonl --decider jev,clef:clef-flash,anthropic
```

The file is JSON Lines. One line lists the rules, and every other line is an email with the number of the rule it should go to (0 for none):

```json
{"rules": [{"id": 1, "name": "Receipts", "intent": "Receipts and invoices for purchases"}]}
{"email": {"from": "orders@shop.example.com", "subject": "Your receipt", "body": "Thanks for your order"}, "expected": 1}
```

An email can also have `from_name`, `to`, `list_id`, `attachments` (file extensions), `is_contact`, `replied_before`, `is_bulk` and `dmarc`, and a rule can have `exceptions`. For each decision model, the report shows accuracy, how often the fallback was asked, latency, the cost per 1,000 emails, and which rules were confused with which.
