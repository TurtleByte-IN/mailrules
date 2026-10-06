// Package settings owns the settings a user changes while the daemon runs: the dry-run
// switch, the decision models and thresholds, retention, and the model provider keys. They
// live in the settings table and lie over the environment (internal/config), which supplies
// the defaults. Provider keys are stored encrypted under the master key and are never
// handed back out of this package except inside the config the model adapters are built from.
package settings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// DefaultRetentionDays is how long message snippets are kept until the user says otherwise.
const DefaultRetentionDays = 30

// maxRetentionDays keeps retention inside what undo promises to cover and a sane upper bound.
const maxRetentionDays = 3650

// keyPrefix marks the settings rows that hold an encrypted provider key.
const keyPrefix = "key."

// Deciders are the values MAILRULES_DECIDER accepts.
var Deciders = []string{"jev", "clef", "anthropic", "openai", "ollama"}

// keyFields maps each provider key's API name to the config field it fills. This is the one
// list of provider keys: the API's "keys" object, validation and the overlay all read it.
var keyFields = map[string]func(*config.Config) *string{
	"openrouter_api_key":    func(c *config.Config) *string { return &c.OpenRouterAPIKey },
	"cloudflare_account_id": func(c *config.Config) *string { return &c.CloudflareAccountID },
	"cloudflare_api_token":  func(c *config.Config) *string { return &c.CloudflareAPIToken },
	"anthropic_api_key":     func(c *config.Config) *string { return &c.AnthropicAPIKey },
	"openai_api_key":        func(c *config.Config) *string { return &c.OpenAIAPIKey },
}

// KeyNames lists the provider keys, sorted.
func KeyNames() []string {
	names := make([]string, 0, len(keyFields))
	for n := range keyFields {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Invalid is a settings change the user has to correct. Path names the field; Message is a
// sentence for a person, complete without the path.
type Invalid struct{ Path, Message string }

func (e *Invalid) Error() string { return e.Path + ": " + e.Message }

// Where a provider key in force comes from. Never the key.
const (
	KeyStored      = "stored"      // saved through the API; it wins over the environment
	KeyEnvironment = "environment" // set in the daemon's environment only
	KeyNone        = "none"
)

// Warning is something about the settings in force that will not work as it stands, which
// the user may still be on the way to fixing (the first-run wizard saves in steps), so it
// is reported and not refused. Path names the setting to fill in.
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Path    string `json:"path"`
}

// View is what may be shown: every setting in force, and for each provider key only
// where it comes from.
type View struct {
	DryRun        bool
	Decider       string
	DeciderModel  string
	FallbackModel string // empty = never escalate
	ComposerModel string
	EscalateBelow float64
	MinConfidence float64
	RetentionDays int
	OpenAIBaseURL string            // an OpenAI-compatible endpoint; empty = api.openai.com
	OllamaURL     string            // a local Ollama server
	Keys          map[string]string // KeyStored | KeyEnvironment | KeyNone
	Warnings      []Warning         // never nil
}

// Patch is a change: nil fields stay as they are. An empty string is a value like any
// other, stored as it is: no fallback, the provider's default model, no URL. Where an
// empty value cannot work (the composer's model, the model of a decider that has no
// default) it is refused. Reset names the settings to forget, which puts the environment's
// default back in force. In Keys, an empty value removes the stored key: a key cannot be
// "set to nothing".
type Patch struct {
	Reset         []string // setting names, as the API spells them
	DryRun        *bool
	Decider       *string
	DeciderModel  *string
	FallbackModel *string
	ComposerModel *string
	EscalateBelow *float64
	MinConfidence *float64
	RetentionDays *int
	OpenAIBaseURL *string // empty = api.openai.com
	OllamaURL     *string // empty = none
	Keys          map[string]string
}

// Settings reads and changes the stored settings.
type Settings struct {
	Store  *store.Store
	Master []byte
	Env    *config.Config // the defaults
	Deps   models.Deps    // shared by every router built here

	mu        sync.Mutex
	print     string // the stored settings the cached routers were built from
	built     bool
	cfg       config.Config // the configuration in force when the routers were built
	router    *models.Router
	overrides map[string]*models.Router // per-rule model overrides, by decider spec; nil = cannot be built
	minCon    float64
}

// ErrNoComposer means no generative model is configured for the rule composer.
var ErrNoComposer = errors.New("no generative model is configured for the rule composer")

// aad binds a key's ciphertext to its name, so rows cannot be swapped.
func aad(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) // #nosec G115 -- bit pattern only
}

// sealed is a provider key as stored: both parts are ciphertext, base64-encoded.
type sealed struct {
	Sealed string `json:"sealed"`
	DEK    string `json:"dek"`
}

func (s *Settings) seal(name, value string) (string, error) {
	secret, dek, err := crypto.Seal(s.Master, aad(name), []byte(value))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sealed{base64.StdEncoding.EncodeToString(secret), base64.StdEncoding.EncodeToString(dek)})
	return string(b), err
}

func (s *Settings) open(name, stored string) (string, error) {
	var v sealed
	if err := json.Unmarshal([]byte(stored), &v); err != nil {
		return "", fmt.Errorf("decode stored key %s: %w", name, err)
	}
	secret, err1 := base64.StdEncoding.DecodeString(v.Sealed)
	dek, err2 := base64.StdEncoding.DecodeString(v.DEK)
	if err := errors.Join(err1, err2); err != nil {
		return "", fmt.Errorf("decode stored key %s: %w", name, err)
	}
	plain, err := crypto.Open(s.Master, aad(name), secret, dek)
	if err != nil {
		return "", fmt.Errorf("stored key %s: %w", name, err)
	}
	return string(plain), nil
}

// effective lays the stored rows over the environment.
func (s *Settings) effective(rows map[string]string) (config.Config, int, error) {
	cfg := *s.Env
	retention := DefaultRetentionDays
	for key, dst := range map[string]any{
		"dry_run": &cfg.DryRun, "decider": &cfg.Decider, "decider_model": &cfg.DeciderModel,
		"fallback_model": &cfg.FallbackModel, "composer_model": &cfg.ComposerModel,
		"escalate_below": &cfg.EscalateBelow, "min_confidence": &cfg.MinConfidence, "retention_days": &retention,
		"openai_base_url": &cfg.OpenAIBaseURL, "ollama_url": &cfg.OllamaURL,
	} {
		if v, ok := rows[key]; ok {
			if err := json.Unmarshal([]byte(v), dst); err != nil {
				return cfg, 0, fmt.Errorf("decode the %s setting: %w", key, err)
			}
		}
	}
	for name, field := range keyFields {
		if v, ok := rows[keyPrefix+name]; ok {
			plain, err := s.open(name, v)
			if err != nil {
				return cfg, 0, err
			}
			*field(&cfg) = plain
		}
	}
	return cfg, retention, nil
}

// Effective returns the configuration in force: the environment with the stored settings
// laid over it. It holds decrypted provider keys, so it is for building model adapters,
// never for showing.
func (s *Settings) Effective(ctx context.Context) (config.Config, error) {
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return config.Config{}, err
	}
	cfg, _, err := s.effective(rows)
	return cfg, err
}

func (s *Settings) view(rows map[string]string, cfg config.Config, retention int) View {
	v := View{DryRun: cfg.DryRun, Decider: cfg.Decider, DeciderModel: cfg.DeciderModel, FallbackModel: cfg.FallbackModel,
		ComposerModel: cfg.ComposerModel, EscalateBelow: cfg.EscalateBelow, MinConfidence: cfg.MinConfidence,
		RetentionDays: retention, OpenAIBaseURL: cfg.OpenAIBaseURL, OllamaURL: cfg.OllamaURL, Keys: map[string]string{},
		Warnings: warnings(cfg)}
	for name, field := range keyFields {
		switch _, stored := rows[keyPrefix+name]; {
		case stored:
			v.Keys[name] = KeyStored
		case *field(s.Env) != "":
			v.Keys[name] = KeyEnvironment
		default:
			v.Keys[name] = KeyNone
		}
	}
	return v
}

// deciderNeeds is what each decider cannot run without: the settings to fill in (as the
// API spells them) and what to call each. config.DeciderReady checks the same fields for
// the startup log; TestWarningsAgreeWithDeciderReady fails if the two lists part ways.
var deciderNeeds = map[string][]struct {
	path, what string
	get        func(*config.Config) string
}{
	"jev":       {{"keys.openrouter_api_key", "an OpenRouter API key", func(c *config.Config) string { return c.OpenRouterAPIKey }}},
	"clef":      {{"keys.cloudflare_account_id", "a Cloudflare account ID", func(c *config.Config) string { return c.CloudflareAccountID }}, {"keys.cloudflare_api_token", "a Cloudflare API token", func(c *config.Config) string { return c.CloudflareAPIToken }}},
	"anthropic": {{"keys.anthropic_api_key", "an Anthropic API key", func(c *config.Config) string { return c.AnthropicAPIKey }}},
	"openai":    {{"keys.openai_api_key", "an OpenAI API key", func(c *config.Config) string { return c.OpenAIAPIKey }}},
	"ollama":    {{"ollama_url", "the URL of your Ollama server", func(c *config.Config) string { return c.OllamaURL }}},
}

// warnings lists what the chosen decider still lacks. It is never nil.
func warnings(cfg config.Config) []Warning {
	out := []Warning{}
	for _, n := range deciderNeeds[cfg.Decider] {
		if n.get(&cfg) == "" {
			out = append(out, Warning{Code: "decider_not_ready", Path: n.path,
				Message: fmt.Sprintf("The %s decision model needs %s. Until it is set, rules that need a model are passed over.", cfg.Decider, n.what)})
		}
	}
	return out
}

// View returns the settings in force.
func (s *Settings) View(ctx context.Context) (View, error) {
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return View{}, err
	}
	cfg, retention, err := s.effective(rows)
	if err != nil {
		return View{}, err
	}
	return s.view(rows, cfg, retention), nil
}

// resettable are the settings Patch.Reset may name.
var resettable = []string{"dry_run", "decider", "decider_model", "fallback_model", "composer_model", "escalate_below",
	"min_confidence", "retention_days", "openai_base_url", "ollama_url"}

// Apply validates a change against the settings it would produce and stores it in one
// transaction. A problem the user can fix comes back as *Invalid. Nothing needs a restart:
// the executor reads dry-run before every action and Live rebuilds the router.
func (s *Settings) Apply(ctx context.Context, p Patch) error {
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return err
	}
	set := map[string]*string{}
	for _, name := range p.Reset {
		if !slices.Contains(resettable, name) {
			return &Invalid{name, "This setting cannot be reset."}
		}
		delete(rows, name) // the environment's default shows through
		set[name] = nil
	}
	cfg, retention, err := s.effective(rows)
	if err != nil {
		return err
	}
	put := func(key string, v any) {
		b, _ := json.Marshal(v) // strings, numbers and booleans cannot fail
		enc := string(b)
		set[key] = &enc
	}
	if p.DryRun != nil {
		cfg.DryRun = *p.DryRun
		put("dry_run", cfg.DryRun)
	}
	for key, f := range map[string]struct{ in, dst *string }{
		"decider": {p.Decider, &cfg.Decider}, "decider_model": {p.DeciderModel, &cfg.DeciderModel},
		"fallback_model": {p.FallbackModel, &cfg.FallbackModel}, "composer_model": {p.ComposerModel, &cfg.ComposerModel},
	} {
		if f.in != nil {
			*f.dst = strings.TrimSpace(*f.in)
			put(key, *f.dst)
		}
	}
	for key, f := range map[string]struct{ in, dst *float64 }{
		"escalate_below": {p.EscalateBelow, &cfg.EscalateBelow}, "min_confidence": {p.MinConfidence, &cfg.MinConfidence},
	} {
		if f.in != nil {
			*f.dst = *f.in
			put(key, *f.dst)
		}
	}
	if p.RetentionDays != nil {
		retention = *p.RetentionDays
		put("retention_days", retention)
	}
	for key, f := range map[string]struct{ in, dst *string }{
		"openai_base_url": {p.OpenAIBaseURL, &cfg.OpenAIBaseURL}, "ollama_url": {p.OllamaURL, &cfg.OllamaURL},
	} {
		if f.in == nil {
			continue
		}
		*f.dst = strings.TrimSpace(*f.in)
		if u, err := url.Parse(*f.dst); *f.dst != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			return &Invalid{key, "Enter an http or https URL, such as http://localhost:11434."}
		}
		put(key, *f.dst)
	}
	for name, value := range p.Keys {
		if keyFields[name] == nil {
			return &Invalid{"keys." + name, "Unknown key. The keys are " + strings.Join(KeyNames(), ", ") + "."}
		}
		if value = strings.TrimSpace(value); value == "" {
			set[keyPrefix+name] = nil
			continue
		}
		enc, err := s.seal(name, value)
		if err != nil {
			return fmt.Errorf("encrypt key %s: %w", name, err)
		}
		set[keyPrefix+name] = &enc
	}

	// The same rules as config.Validate, worded for a form field.
	switch {
	case !slices.Contains(Deciders, cfg.Decider):
		return &Invalid{"decider", "The decision model must be one of " + strings.Join(Deciders, ", ") + "."}
	case (cfg.Decider == "openai" || cfg.Decider == "ollama") && cfg.DeciderModel == "":
		return &Invalid{"decider_model", "The " + cfg.Decider + " decider has no default model. Name one."}
	case cfg.ComposerModel == "":
		return &Invalid{"composer_model", "The rule composer needs a model."}
	case cfg.EscalateBelow < 0 || cfg.EscalateBelow > 1:
		return &Invalid{"escalate_below", "The escalation threshold must be between 0 and 1."}
	case cfg.MinConfidence < 0 || cfg.MinConfidence > 1:
		return &Invalid{"min_confidence", "The confidence threshold must be between 0 and 1."}
	case retention < 1 || retention > maxRetentionDays:
		return &Invalid{"retention_days", fmt.Sprintf("Retention must be between 1 and %d days.", maxRetentionDays)}
	}
	if err := cfg.Validate(); err != nil { // whatever config.Validate learns to check later
		return &Invalid{"", "These settings cannot be used together: " + err.Error() + "."}
	}
	return s.Store.SetSettings(ctx, set)
}

// Live returns the decision router and the act threshold for the settings as stored right
// now; pipeline.Pipeline.Live is this method. The router is rebuilt only when a stored
// setting has changed since the last call, whoever changed it, so a save in the browser
// reaches the next email without a restart. router is nil while the decider lacks its key.
func (s *Settings) Live(ctx context.Context) (router *models.Router, minConfidence float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read settings; keeping the last ones", "error", err.Error())
		if !s.built {
			return nil, s.Env.MinConfidence
		}
		return s.router, s.minCon
	}
	keys := make([]string, 0, len(rows))
	for k := range rows {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "\x00" + rows[k] + "\x00")
	}
	if s.built && b.String() == s.print {
		return s.router, s.minCon
	}
	cfg, _, err := s.effective(rows)
	if err != nil {
		slog.WarnContext(ctx, "stored settings cannot be used; mail that needs a model waits in Needs review", "error", err.Error())
		cfg = *s.Env
	}
	s.router, s.overrides, s.cfg = nil, map[string]*models.Router{}, cfg
	if ready := cfg.DeciderReady(); ready != nil {
		slog.WarnContext(ctx, "no decision model yet; new mail that needs one waits in Needs review until it is set", "missing", ready.Error())
	} else if s.router, err = models.NewRouter(&cfg, cfg.DeciderSpec(), s.Deps, s.Store); err != nil {
		slog.WarnContext(ctx, "could not set up the decision model", "error", err.Error())
		s.router = nil
	}
	s.print, s.built, s.minCon = b.String(), true, cfg.MinConfidence
	return s.router, s.minCon
}

// CheckModel says, in a sentence, what is wrong with a rule's model override, or "" when it
// is a decider this daemon knows: empty (the default), a decider name, or name:model.
// openai and ollama have no default model.
func CheckModel(spec string) string {
	if spec == "" {
		return ""
	}
	name, model, _ := strings.Cut(spec, ":")
	if !slices.Contains(Deciders, name) {
		return fmt.Sprintf("The model must be empty, or one of %s, optionally followed by :model.", strings.Join(Deciders, ", "))
	}
	if (name == "openai" || name == "ollama") && model == "" {
		return fmt.Sprintf("The %s decider has no default model. Write it as %s:<model>.", name, name)
	}
	return ""
}

// RouterFor returns the router for a rule's model override (a decider spec, see
// CheckModel), built from the settings in force and kept until they change. It is nil
// when that decider cannot be used, for example because its key is not set; the caller
// then decides with the default router. pipeline.Pipeline.Override is this method.
func (s *Settings) RouterFor(ctx context.Context, spec string) *models.Router {
	s.Live(ctx) // rebuilds, and forgets the overrides, when a stored setting changed
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.overrides[spec]; ok {
		return r
	}
	r, err := models.NewRouter(&s.cfg, spec, s.Deps, s.Store)
	if err != nil {
		slog.WarnContext(ctx, "a rule's model cannot be used; the default decides instead", "model", spec, "error", err.Error())
		r = nil
	}
	if s.overrides == nil {
		s.overrides = map[string]*models.Router{}
	}
	s.overrides[spec] = r
	return r
}

// Composer returns the generative model the rule composer writes rules with:
// composer_model on Anthropic. It is ErrNoComposer until an Anthropic key is set.
func (s *Settings) Composer(ctx context.Context) (models.Generator, error) {
	cfg, err := s.Effective(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.AnthropicAPIKey == "" || cfg.ComposerModel == "" {
		return nil, ErrNoComposer
	}
	return models.NewAnthropic("", cfg.AnthropicAPIKey, cfg.ComposerModel, s.Deps), nil
}

// RetentionDays returns the retention_days setting in force: how long message snippets
// are kept. When the settings cannot be read it is the default.
func (s *Settings) RetentionDays(ctx context.Context) int {
	v, err := s.View(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read the retention setting; using the default", "error", err.Error())
		return DefaultRetentionDays
	}
	return v.RetentionDays
}
