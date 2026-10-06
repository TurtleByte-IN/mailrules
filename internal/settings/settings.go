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

// Invalid is a settings change the user has to correct. Path names the field.
type Invalid struct{ Path, Message string }

func (e *Invalid) Error() string { return e.Path + ": " + e.Message }

// View is what may be shown: every setting in force, and for each provider key only
// whether one is set.
type View struct {
	DryRun        bool
	Decider       string
	DeciderModel  string
	FallbackModel string // empty = never escalate
	ComposerModel string
	EscalateBelow float64
	MinConfidence float64
	RetentionDays int
	Keys          map[string]bool
}

// Patch is a change: nil fields stay as they are. In Keys, an empty value removes the
// stored key, which puts the environment's back in force.
type Patch struct {
	DryRun        *bool
	Decider       *string
	DeciderModel  *string
	FallbackModel *string
	ComposerModel *string
	EscalateBelow *float64
	MinConfidence *float64
	RetentionDays *int
	Keys          map[string]string
}

// Settings reads and changes the stored settings.
type Settings struct {
	Store  *store.Store
	Master []byte
	Env    *config.Config // the defaults
	Deps   models.Deps    // shared by every router built here

	mu     sync.Mutex
	print  string // the stored settings the cached router was built from
	built  bool
	router *models.Router
	minCon float64
}

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

func view(cfg config.Config, retention int) View {
	v := View{DryRun: cfg.DryRun, Decider: cfg.Decider, DeciderModel: cfg.DeciderModel, FallbackModel: cfg.FallbackModel,
		ComposerModel: cfg.ComposerModel, EscalateBelow: cfg.EscalateBelow, MinConfidence: cfg.MinConfidence,
		RetentionDays: retention, Keys: map[string]bool{}}
	for name, field := range keyFields {
		v.Keys[name] = *field(&cfg) != ""
	}
	return v
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
	return view(cfg, retention), nil
}

// Apply validates a change against the settings it would produce and stores it in one
// transaction. A problem the user can fix comes back as *Invalid. Nothing needs a restart:
// the executor reads dry-run before every action and Live rebuilds the router.
func (s *Settings) Apply(ctx context.Context, p Patch) error {
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return err
	}
	cfg, retention, err := s.effective(rows)
	if err != nil {
		return err
	}
	set := map[string]*string{}
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
	for name, value := range p.Keys {
		if keyFields[name] == nil {
			return &Invalid{"keys." + name, "unknown key; the keys are " + strings.Join(KeyNames(), ", ")}
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
		return &Invalid{"decider", "must be " + strings.Join(Deciders, ", ")}
	case (cfg.Decider == "openai" || cfg.Decider == "ollama") && cfg.DeciderModel == "":
		return &Invalid{"decider_model", cfg.Decider + " has no default model: name one"}
	case cfg.ComposerModel == "":
		return &Invalid{"composer_model", "the rule composer needs a model"}
	case cfg.EscalateBelow < 0 || cfg.EscalateBelow > 1:
		return &Invalid{"escalate_below", "must be between 0 and 1"}
	case cfg.MinConfidence < 0 || cfg.MinConfidence > 1:
		return &Invalid{"min_confidence", "must be between 0 and 1"}
	case retention < 1 || retention > maxRetentionDays:
		return &Invalid{"retention_days", fmt.Sprintf("must be between 1 and %d", maxRetentionDays)}
	}
	if err := cfg.Validate(); err != nil { // whatever config.Validate learns to check later
		return &Invalid{"", err.Error()}
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
	s.router = nil
	if ready := cfg.DeciderReady(); ready != nil {
		slog.WarnContext(ctx, "no decision model yet; new mail that needs one waits in Needs review until it is set", "missing", ready.Error())
	} else if s.router, err = models.NewRouter(&cfg, cfg.DeciderSpec(), s.Deps, s.Store); err != nil {
		slog.WarnContext(ctx, "could not set up the decision model", "error", err.Error())
		s.router = nil
	}
	s.print, s.built, s.minCon = b.String(), true, cfg.MinConfidence
	return s.router, s.minCon
}
