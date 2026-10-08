// Package config loads daemon settings from environment variables and flags.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
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
	// AnthropicWorkspaceID names the workspace every Claude request runs in (the
	// anthropic-workspace-id header). Only a key that covers a whole organisation needs it;
	// empty = none sent.
	AnthropicWorkspaceID string
	OpenAIBaseURL        string
	OpenAIAPIKey         string
	OllamaURL            string

	PricesFile string
	LogLevel   string

	// CookieSecure is auto, true or false; see SecureCookies.
	CookieSecure string

	// TrustedProxies lists the reverse proxies whose X-Forwarded-For header is believed,
	// as IP addresses or CIDR ranges separated by commas; see TrustedProxyPrefixes.
	TrustedProxies string

	// The outgoing mail server the summary email is sent through. SMTPPort 0 means the
	// usual port for SMTPTLS; SMTPPasswordFile names a file holding the password, like
	// MasterKeyFile. Never logged.
	SMTPHost         string
	SMTPPort         int
	SMTPUser         string
	SMTPPassword     string
	SMTPPasswordFile string
	SMTPFrom         string
	SMTPTLS          string // starttls | implicit | none (none: localhost only)

	// PublicURL is where the web app is reached from the user's mail program: links in the
	// summary email start with it.
	PublicURL string
}

// flagName turns MAILRULES_DATA_DIR into data-dir and OPENROUTER_API_KEY into openrouter-api-key.
func flagName(env string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimPrefix(env, "MAILRULES_")), "_", "-")
}

// Setting describes one setting as the settings reference (docs/guide/settings.md) lists it.
type Setting struct {
	Group   string // the heading it is listed under
	Env     string // the environment variable
	Flag    string // the command-line flag, without the leading --
	Default string // the default as the flag package prints it; empty for none
	Help    string // what it does, for a reader of the reference
}

// Settings lists every setting in the order the reference shows them.
func Settings() []Setting {
	return define(&Config{}, flag.NewFlagSet("mailrules", flag.ContinueOnError))
}

// define registers every setting on fs, bound to c's fields, and lists them. It is the one
// place a setting's name, default and help are written.
func define(c *Config, fs *flag.FlagSet) []Setting {
	var list []Setting
	group := ""
	add := func(env string) {
		f := fs.Lookup(flagName(env))
		list = append(list, Setting{Group: group, Env: env, Flag: f.Name, Default: f.DefValue, Help: f.Usage})
	}
	str := func(p *string, env, def, help string) {
		fs.StringVar(p, flagName(env), def, help)
		add(env)
	}
	boolean := func(p *bool, env string, def bool, help string) {
		fs.BoolVar(p, flagName(env), def, help)
		add(env)
	}
	integer := func(p *int, env string, def int, help string) {
		fs.IntVar(p, flagName(env), def, help)
		add(env)
	}
	float := func(p *float64, env string, def float64, help string) {
		fs.Float64Var(p, flagName(env), def, help)
		add(env)
	}

	// The help strings are written for docs/guide/settings.md, which is Markdown: code in
	// backticks. Run `make settings-doc` after changing any of them.
	group = "Daemon"
	str(&c.DataDir, "MAILRULES_DATA_DIR", "./data", "Directory for the database (`mailrules.db`) and the generated `master.key`. A relative path is relative to the directory MailRules starts in.")
	str(&c.Listen, "MAILRULES_LISTEN", "127.0.0.1:8080", "Address (`host:port`) the web UI and API listen on. Any address other than a loopback one makes MailRules log a warning: put a reverse proxy with TLS in front.")
	str(&c.Mode, "MAILRULES_MODE", "selfhost", "`selfhost` or `cloud`. `cloud` is for the hosted service and only hides the Self-hosting card in Settings; leave it as `selfhost`.")
	str(&c.MasterKey, "MAILRULES_MASTER_KEY", "", "The master key itself: 32 bytes, base64-encoded. It encrypts the stored mailbox passwords and provider keys. Set this or `MAILRULES_MASTER_KEY_FILE`, not both; with neither, `master.key` in the data directory is used, and generated on first run.")
	str(&c.MasterKeyFile, "MAILRULES_MASTER_KEY_FILE", "", "A file holding the master key, to keep it outside the data directory. MailRules does not start if the file is missing.")
	boolean(&c.DryRun, "MAILRULES_DRY_RUN", true, "Dry-run until it is first switched: decisions are recorded and no mailbox is changed. Once dry-run has been switched in the browser or with `mailrules dry-run on|off`, that choice wins.")
	str(&c.LogLevel, "LOG_LEVEL", "info", "`debug`, `info`, `warn` or `error`. Logs are JSON lines on standard error and never hold passwords, keys or the text of an email.")

	group = "Decision models"
	str(&c.Decider, "MAILRULES_DECIDER", "jev", "The decision model, which picks the rule an email matches: `jev`, `clef`, `anthropic`, `openai` or `ollama`.")
	str(&c.DeciderModel, "MAILRULES_DECIDER_MODEL", "", "The decision model's model name. Empty uses the provider's default; `openai` and `ollama` have none, so they need one.")
	str(&c.FallbackModel, "MAILRULES_FALLBACK_MODEL", "claude-haiku-4-5", "The Claude model asked for a second opinion when the decision model is unsure. It needs `ANTHROPIC_API_KEY`. Empty turns the fallback off.")
	str(&c.ComposerModel, "MAILRULES_COMPOSER_MODEL", "claude-haiku-4-5", "The model that writes rules from your words: a Claude model, or `openai:<model>` or `ollama:<model>`.")
	float(&c.EscalateBelow, "MAILRULES_ESCALATE_BELOW", 0.75, "When the decision model's confidence is below this (0 to 1), the fallback model is asked too.")
	float(&c.MinConfidence, "MAILRULES_MIN_CONFIDENCE", 0.75, "The confidence (0 to 1) a decision needs before a rule acts, unless the rule sets its own. Below it, the email waits in Needs review.")
	integer(&c.BodyChars, "MAILRULES_BODY_CHARS", 2000, "How many characters of an email's plain text are sent to a model.")
	integer(&c.ModelConcurrency, "MAILRULES_MODEL_CONCURRENCY", 8, "How many model calls may be in flight at once.")
	str(&c.PricesFile, "MAILRULES_PRICES_FILE", "", "A JSON file of per-model prices, in USD per million tokens, laid over the built-in table the cost estimate uses.")

	group = "Provider keys"
	str(&c.OpenRouterAPIKey, "OPENROUTER_API_KEY", "", "OpenRouter API key, for Jev.")
	str(&c.CloudflareAccountID, "CLOUDFLARE_ACCOUNT_ID", "", "Cloudflare account ID, for Clef.")
	str(&c.CloudflareAPIToken, "CLOUDFLARE_API_TOKEN", "", "Cloudflare API token, for Clef.")
	str(&c.AnthropicAPIKey, "ANTHROPIC_API_KEY", "", "Anthropic API key, for the fallback model, for the rule composer when its model is Claude, and for the `anthropic` decision model.")
	str(&c.AnthropicWorkspaceID, "ANTHROPIC_WORKSPACE_ID", "", "The Claude workspace (`wrkspc_…`) requests run in. Only a key that covers a whole organisation needs it.")
	str(&c.OpenAIBaseURL, "OPENAI_BASE_URL", "", "Base URL of an OpenAI-compatible endpoint. Empty means OpenAI itself.")
	str(&c.OpenAIAPIKey, "OPENAI_API_KEY", "", "API key for the OpenAI-compatible endpoint.")
	str(&c.OllamaURL, "OLLAMA_URL", "", "URL of an Ollama server, such as `http://localhost:11434`.")

	group = "Web UI"
	str(&c.CookieSecure, "MAILRULES_COOKIE_SECURE", "auto", "Send the login cookie over HTTPS only: `auto` (yes unless MailRules listens on loopback only), `true` or `false`. Set `true` behind a TLS proxy; `false` only when the UI is opened over plain HTTP on this machine, as with Docker.")
	str(&c.TrustedProxies, "MAILRULES_TRUSTED_PROXIES", "", "Reverse proxies whose `X-Forwarded-For` header MailRules believes, as IP addresses or CIDR ranges separated by commas, such as `127.0.0.1,172.18.0.0/16`. Sign-in limits then count the real visitor instead of the proxy. Empty trusts no one, which is right when nothing sits in front of MailRules; never list a range that visitors can reach directly.")
	str(&c.PublicURL, "MAILRULES_PUBLIC_URL", "http://127.0.0.1:8080", "Where you open MailRules; links in the summary email start with it. Set it to your proxy's address, such as `https://mailrules.example.com`.")

	group = "Summary email"
	str(&c.SMTPHost, "MAILRULES_SMTP_HOST", "", "Outgoing mail server the summary email is sent through, such as `smtp.example.com`, without a scheme or port.")
	integer(&c.SMTPPort, "MAILRULES_SMTP_PORT", 0, "Its port. `0` uses the usual one: 587 for `starttls`, 465 for `implicit`, 25 for `none`.")
	str(&c.SMTPUser, "MAILRULES_SMTP_USER", "", "User name to sign in to the mail server with. Empty sends without signing in.")
	str(&c.SMTPPassword, "MAILRULES_SMTP_PASSWORD", "", "Its password; an app password where the provider has them.")
	str(&c.SMTPPasswordFile, "MAILRULES_SMTP_PASSWORD_FILE", "", "A file holding that password, instead of `MAILRULES_SMTP_PASSWORD`.")
	str(&c.SMTPFrom, "MAILRULES_SMTP_FROM", "", "The summary's From address, such as `MailRules <me@example.com>`. Most servers want one of your own addresses.")
	str(&c.SMTPTLS, "MAILRULES_SMTP_TLS", "starttls", "`starttls` (the server must offer it), `implicit` (TLS from the start), or `none`, which is allowed only for a mail server on this machine.")
	return list
}

// Load reads settings from the environment, then lets flags in args override them.
// It does not validate; call Validate before serving.
func Load(args []string, getenv func(string) string) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("mailrules", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var errs []error
	for _, s := range define(c, fs) {
		if v := getenv(s.Env); v != "" {
			if err := fs.Set(s.Flag, v); err != nil {
				errs = append(errs, fmt.Errorf("%s=%q is not valid: %w", s.Env, v, err))
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
	if !ValidWorkspaceID(c.AnthropicWorkspaceID) {
		bad("ANTHROPIC_WORKSPACE_ID=%q must start with wrkspc_ and hold only letters and digits after it", c.AnthropicWorkspaceID)
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
	if _, err := c.TrustedProxyPrefixes(); err != nil {
		bad("%v", err)
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		bad("LOG_LEVEL=%q must be debug, info, warn or error", c.LogLevel)
	}
	errs = append(errs, c.validateSMTP()...)
	return errors.Join(errs...)
}

// SMTP TLS modes, as MAILRULES_SMTP_TLS takes them.
const (
	SMTPStartTLS = "starttls" // plain connection upgraded with STARTTLS, which the server must offer
	SMTPImplicit = "implicit" // TLS from the first byte (SMTPS)
	SMTPNoTLS    = "none"     // no encryption: only for a mail server on this machine
)

// validateSMTP checks the summary email's settings that are set. What is missing is not an
// error here: the daemon runs without a mail server, and SMTPMissing says what to add.
func (c *Config) validateSMTP() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.SMTPHost != "" && (strings.ContainsAny(c.SMTPHost, "/:@ \t\r\n") && net.ParseIP(c.SMTPHost) == nil) {
		bad("MAILRULES_SMTP_HOST=%q must be a host name or IP address, without a scheme or port (the port is MAILRULES_SMTP_PORT)", c.SMTPHost)
	}
	if c.SMTPPort < 0 || c.SMTPPort > 65535 {
		bad("MAILRULES_SMTP_PORT=%d must be between 1 and 65535, or 0 for the usual port", c.SMTPPort)
	}
	switch c.SMTPTLS {
	case SMTPStartTLS, SMTPImplicit:
	case SMTPNoTLS:
		if c.SMTPHost != "" && !isLocalHost(c.SMTPHost) {
			bad("MAILRULES_SMTP_TLS=none sends the password and the summary unencrypted, so it is allowed only for a mail server on this machine (localhost), not %q", c.SMTPHost)
		}
	default:
		bad("MAILRULES_SMTP_TLS=%q must be starttls, implicit or none", c.SMTPTLS)
	}
	if c.SMTPPassword != "" && c.SMTPPasswordFile != "" {
		bad("set only one of MAILRULES_SMTP_PASSWORD and MAILRULES_SMTP_PASSWORD_FILE")
	}
	if c.SMTPFrom != "" {
		if _, err := mail.ParseAddress(c.SMTPFrom); err != nil || strings.ContainsAny(c.SMTPFrom, "\r\n") {
			bad("MAILRULES_SMTP_FROM=%q must be one email address, such as mailrules@example.com or MailRules <mailrules@example.com>", c.SMTPFrom)
		}
	}
	if u, err := url.Parse(c.PublicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		bad("MAILRULES_PUBLIC_URL=%q must be an http or https URL with no query or #, such as https://mail.example.com", c.PublicURL)
	}
	return errs
}

// isLocalHost reports whether host names this machine.
func isLocalHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SMTPMissing lists the environment variables still to set before the summary email can be
// sent, in the order to set them; empty when it can be. A user name needs a password and a
// password a user name.
func (c *Config) SMTPMissing() []string {
	missing := []string{}
	if c.SMTPHost == "" {
		missing = append(missing, "MAILRULES_SMTP_HOST")
	}
	if c.SMTPFrom == "" {
		missing = append(missing, "MAILRULES_SMTP_FROM")
	}
	hasPassword := c.SMTPPassword != "" || c.SMTPPasswordFile != ""
	switch {
	case c.SMTPUser != "" && !hasPassword:
		missing = append(missing, "MAILRULES_SMTP_PASSWORD")
	case c.SMTPUser == "" && hasPassword:
		missing = append(missing, "MAILRULES_SMTP_USER")
	}
	return missing
}

// SMTPAddr is the mail server's host:port, with the usual port for the TLS mode when none is set.
func (c *Config) SMTPAddr() string {
	port := c.SMTPPort
	if port == 0 {
		port = map[string]int{SMTPStartTLS: 587, SMTPImplicit: 465, SMTPNoTLS: 25}[c.SMTPTLS]
	}
	return net.JoinHostPort(c.SMTPHost, fmt.Sprint(port))
}

// SMTPSecret returns the mail server's password: MAILRULES_SMTP_PASSWORD, or the contents of
// MAILRULES_SMTP_PASSWORD_FILE without its line ending.
func (c *Config) SMTPSecret() (string, error) {
	if c.SMTPPasswordFile == "" {
		return c.SMTPPassword, nil
	}
	b, err := os.ReadFile(c.SMTPPasswordFile) // #nosec G304 -- operator-chosen path
	if err != nil {
		return "", fmt.Errorf("read MAILRULES_SMTP_PASSWORD_FILE: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// DeciderSpec is the decider as models.NewRouter takes it: "name" or "name:model".
func (c *Config) DeciderSpec() string {
	if c.DeciderModel == "" {
		return c.Decider
	}
	return c.Decider + ":" + c.DeciderModel
}

// workspacePrefix starts every Anthropic workspace ID.
const workspacePrefix = "wrkspc_"

// ValidWorkspaceID reports whether id can name an Anthropic workspace: empty (none), or
// wrkspc_ followed by letters and digits, as the Claude Console shows them. Nothing else
// may reach the anthropic-workspace-id header.
func ValidWorkspaceID(id string) bool {
	if id == "" {
		return true
	}
	rest, ok := strings.CutPrefix(id, workspacePrefix)
	if !ok || rest == "" {
		return false
	}
	for _, r := range rest {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
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

// TrustedProxyPrefixes parses MAILRULES_TRUSTED_PROXIES: IP addresses and CIDR ranges
// separated by commas or spaces. A bare address names just itself. A range that covers
// every address (/0) is refused: it would let anyone forge X-Forwarded-For.
func (c *Config) TrustedProxyPrefixes() ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.FieldsFunc(c.TrustedProxies, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		var p netip.Prefix
		if strings.Contains(f, "/") {
			var err error
			if p, err = netip.ParsePrefix(f); err != nil {
				return nil, fmt.Errorf("MAILRULES_TRUSTED_PROXIES: %q is not an IP address or a CIDR range such as 10.0.0.0/8", f)
			}
		} else {
			a, err := netip.ParseAddr(f)
			if err != nil {
				return nil, fmt.Errorf("MAILRULES_TRUSTED_PROXIES: %q is not an IP address or a CIDR range such as 10.0.0.0/8", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("MAILRULES_TRUSTED_PROXIES: %q would trust every address, which lets anyone forge X-Forwarded-For; list only your proxies", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// ListensLocally reports whether the HTTP address is loopback-only.
func (c *Config) ListensLocally() bool {
	host, _, err := net.SplitHostPort(c.Listen)
	return err == nil && isLocalHost(host)
}
