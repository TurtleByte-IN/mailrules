// Package config loads daemon settings from environment variables and flags.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
)

// Config holds every setting from the backend plan's Configuration table.
type Config struct {
	DataDir       string
	Listen        string
	Mode          string
	MasterKey     string
	MasterKeyFile string
	DryRun        bool

	Decider          string
	DeciderModel     string
	FallbackModel    string
	ComposerModel    string
	EscalateBelow    float64
	MinConfidence    float64
	BodyChars        int
	ModelConcurrency int

	OpenRouterAPIKey    string
	CloudflareAccountID string
	CloudflareAPIToken  string
	AnthropicAPIKey     string
	OpenAIBaseURL       string
	OpenAIAPIKey        string
	OllamaURL           string

	PricesFile string
	LogLevel   string

	// CookieSecure is auto, true or false; see SecureCookies.
	CookieSecure string
}

// flagName turns MAILRULES_DATA_DIR into data-dir and OPENROUTER_API_KEY into openrouter-api-key.
func flagName(env string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(env, "MAILRULES_")), "_", "-")
}

// Load reads settings from the environment, then lets flags in args override them.
// It does not validate; call Validate before serving.
func Load(args []string, getenv func(string) string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("mailrules", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var envs []string
	str := func(p *string, env, def, usage string) {
		fs.StringVar(p, flagName(env), def, usage+" (env "+env+")")
		envs = append(envs, env)
	}
	str(&c.DataDir, "MAILRULES_DATA_DIR", "./data", "SQLite file, logs, generated keys")
	str(&c.Listen, "MAILRULES_LISTEN", "127.0.0.1:8080", "HTTP address")
	str(&c.Mode, "MAILRULES_MODE", "selfhost", "selfhost or cloud")
	str(&c.MasterKey, "MAILRULES_MASTER_KEY", "", "32-byte base64 key-encryption key")
	str(&c.MasterKeyFile, "MAILRULES_MASTER_KEY_FILE", "", "file holding the master key")
	fs.BoolVar(&c.DryRun, flagName("MAILRULES_DRY_RUN"), true, "log decisions without changing mailboxes (env MAILRULES_DRY_RUN)")
	envs = append(envs, "MAILRULES_DRY_RUN")
	str(&c.Decider, "MAILRULES_DECIDER", "jev", "jev, clef, anthropic, openai or ollama")
	str(&c.DeciderModel, "MAILRULES_DECIDER_MODEL", "", "the decider's model; empty = the provider's default (openai and ollama have none)")
	str(&c.FallbackModel, "MAILRULES_FALLBACK_MODEL", "claude-haiku-4-5", "model used when the decider is unsure; empty disables escalation")
	str(&c.ComposerModel, "MAILRULES_COMPOSER_MODEL", "claude-haiku-4-5", "generative model for the rule composer: a Claude model, or openai:<model> or ollama:<model>")
	fs.Float64Var(&c.EscalateBelow, flagName("MAILRULES_ESCALATE_BELOW"), 0.75, "decider confidence below this escalates (env MAILRULES_ESCALATE_BELOW)")
	fs.Float64Var(&c.MinConfidence, flagName("MAILRULES_MIN_CONFIDENCE"), 0.75, "default act threshold (env MAILRULES_MIN_CONFIDENCE)")
	fs.IntVar(&c.BodyChars, flagName("MAILRULES_BODY_CHARS"), 2000, "plain-text characters sent to models (env MAILRULES_BODY_CHARS)")
	fs.IntVar(&c.ModelConcurrency, flagName("MAILRULES_MODEL_CONCURRENCY"), 8, "cap on in-flight model calls (env MAILRULES_MODEL_CONCURRENCY)")
	envs = append(envs, "MAILRULES_ESCALATE_BELOW", "MAILRULES_MIN_CONFIDENCE", "MAILRULES_BODY_CHARS", "MAILRULES_MODEL_CONCURRENCY")
	str(&c.OpenRouterAPIKey, "OPENROUTER_API_KEY", "", "Jev via OpenRouter")
	str(&c.CloudflareAccountID, "CLOUDFLARE_ACCOUNT_ID", "", "Clef")
	str(&c.CloudflareAPIToken, "CLOUDFLARE_API_TOKEN", "", "Clef")
	str(&c.AnthropicAPIKey, "ANTHROPIC_API_KEY", "", "Haiku fallback, and the composer when its model is Claude")
	str(&c.OpenAIBaseURL, "OPENAI_BASE_URL", "", "any OpenAI-compatible endpoint")
	str(&c.OpenAIAPIKey, "OPENAI_API_KEY", "", "any OpenAI-compatible endpoint")
	str(&c.OllamaURL, "OLLAMA_URL", "", "local models")
	str(&c.PricesFile, "MAILRULES_PRICES_FILE", "", "per-model prices for the cost ledger")
	str(&c.LogLevel, "LOG_LEVEL", "info", "debug, info, warn or error")
	str(&c.CookieSecure, "MAILRULES_COOKIE_SECURE", "auto", "send login cookies over HTTPS only: auto, true or false")

	var errs []error
	for _, env := range envs {
		if v := getenv(env); v != "" {
			if err := fs.Set(flagName(env), v); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not valid: %w", env, v, err))
			}
		}
	}
	if err := fs.Parse(args); err != nil {
		errs = append(errs, fmt.Errorf("parse flags: %w", err))
	}
	return c, errors.Join(errs...)
}

// Validate reports every problem at once, one message per problem.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.DataDir == "" {
		bad("MAILRULES_DATA_DIR is empty")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		bad("MAILRULES_LISTEN=%q is not host:port", c.Listen)
	}
	if c.Mode != "selfhost" && c.Mode != "cloud" {
		bad("MAILRULES_MODE=%q must be selfhost or cloud", c.Mode)
	}
	if c.MasterKey != "" && c.MasterKeyFile != "" {
		bad("set only one of MAILRULES_MASTER_KEY and MAILRULES_MASTER_KEY_FILE")
	}
	switch c.Decider {
	case "jev", "clef", "anthropic", "openai", "ollama":
	default:
		bad("MAILRULES_DECIDER=%q must be jev, clef, anthropic, openai or ollama", c.Decider)
	}
	if (c.Decider == "openai" || c.Decider == "ollama") && c.DeciderModel == "" {
		bad("MAILRULES_DECIDER=%s needs MAILRULES_DECIDER_MODEL: that provider has no default model", c.Decider)
	}
	if c.ComposerModel != "" { // empty is the settings' business: they refuse it on save
		if _, _, err := SplitComposerModel(c.ComposerModel); err != nil {
			bad("MAILRULES_COMPOSER_MODEL=%q: %v", c.ComposerModel, err)
		}
	}
	if c.EscalateBelow < 0 || c.EscalateBelow > 1 {
		bad("MAILRULES_ESCALATE_BELOW=%v must be between 0 and 1", c.EscalateBelow)
	}
	if c.MinConfidence < 0 || c.MinConfidence > 1 {
		bad("MAILRULES_MIN_CONFIDENCE=%v must be between 0 and 1", c.MinConfidence)
	}
	if c.BodyChars <= 0 {
		bad("MAILRULES_BODY_CHARS=%d must be positive", c.BodyChars)
	}
	if c.ModelConcurrency <= 0 {
		bad("MAILRULES_MODEL_CONCURRENCY=%d must be positive", c.ModelConcurrency)
	}
	switch c.CookieSecure {
	case "auto", "true", "false":
	default:
		bad("MAILRULES_COOKIE_SECURE=%q must be auto, true or false", c.CookieSecure)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("LOG_LEVEL=%q must be debug, info, warn or error", c.LogLevel)
	}
	return errors.Join(errs...)
}

// DeciderSpec is the decider as models.NewRouter takes it: "name" or "name:model".
func (c *Config) DeciderSpec() string {
	if c.DeciderModel == "" {
		return c.Decider
	}
	return c.Decider + ":" + c.DeciderModel
}

// ComposerProviders are the providers the rule composer can write with.
var ComposerProviders = []string{"anthropic", "openai", "ollama"}

// MissingModel is the error for a provider spec that names a provider with no default
// model and no model: role is what the spec is for ("decider", "composer").
func MissingModel(role, provider string) error {
	return fmt.Errorf("%s %s needs a model: write it as %s:<model>", role, provider, provider)
}

// SplitComposerModel reads MAILRULES_COMPOSER_MODEL (the composer_model setting): a bare
// model name is a Claude model on Anthropic, as it always was; provider:model names the
// provider (anthropic, openai or ollama), and everything after the first colon is the
// model, so ollama:llama3.2:3b is the model llama3.2:3b. On error provider and model are
// still what was written, so a caller can tell a missing model from an unknown provider.
func SplitComposerModel(spec string) (provider, model string, err error) {
	name, model, found := strings.Cut(spec, ":")
	switch {
	case !found && name == "":
		return "", "", errors.New("the rule composer needs a model")
	case !found:
		return "anthropic", name, nil
	case !slices.Contains(ComposerProviders, name):
		return name, model, fmt.Errorf("unknown composer provider %q: must be %s", name, strings.Join(ComposerProviders, ", "))
	case model == "":
		return name, "", MissingModel("composer", name)
	}
	return name, model, nil
}

// DeciderReady reports what the chosen decider still needs before it can run.
// It is not part of Validate: a fresh install starts without a key so the
// first-run wizard can collect one, and mail waits for review until then.
func (c *Config) DeciderReady() error {
	return c.providerReady("MAILRULES_DECIDER="+c.Decider, c.Decider)
}

// ComposerReady is DeciderReady for the rule composer's model.
func (c *Config) ComposerReady() error {
	provider, _, err := SplitComposerModel(c.ComposerModel)
	if err != nil {
		return fmt.Errorf("MAILRULES_COMPOSER_MODEL=%q: %w", c.ComposerModel, err)
	}
	return c.providerReady("MAILRULES_COMPOSER_MODEL="+c.ComposerModel, provider)
}

// providerReady reports what provider lacks; chosen says which setting chose it.
func (c *Config) providerReady(chosen, provider string) error {
	var errs []error
	needs := func(env, val string) {
		if val == "" {
			errs = append(errs, fmt.Errorf("%s but %s is empty", chosen, env))
		}
	}
	switch provider {
	case "jev":
		needs("OPENROUTER_API_KEY", c.OpenRouterAPIKey)
	case "clef":
		needs("CLOUDFLARE_ACCOUNT_ID", c.CloudflareAccountID)
		needs("CLOUDFLARE_API_TOKEN", c.CloudflareAPIToken)
	case "anthropic":
		needs("ANTHROPIC_API_KEY", c.AnthropicAPIKey)
	case "openai":
		needs("OPENAI_API_KEY", c.OpenAIAPIKey)
	case "ollama":
		needs("OLLAMA_URL", c.OllamaURL)
	}
	return errors.Join(errs...)
}

// SecureCookies reports whether login cookies are marked HTTPS-only.
// "auto" means yes unless the daemon listens on loopback only. Set "false" when
// the daemon listens on every interface but is only published to this machine
// over plain HTTP (the Docker Compose setup); set "true" behind a TLS proxy.
func (c *Config) SecureCookies() bool {
	switch c.CookieSecure {
	case "true":
		return true
	case "false":
		return false
	}
	return !c.ListensLocally()
}

// ListensLocally reports whether the HTTP address is loopback-only.
func (c *Config) ListensLocally() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
