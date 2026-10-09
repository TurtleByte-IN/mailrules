// Package settings owns the settings a user changes while the daemon runs: the dry-run
// switch, the decision models and thresholds, retention, where trashed mail goes, whether
// the mailbox's own mail is left alone, and the model provider keys. They live in the
// settings table and lie over the environment (internal/config), which supplies the
// defaults. Provider keys are stored encrypted under the master key and are never handed
// back out of this package except inside the config the model adapters are built from.
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
	// FallbackActive says the fallback will really be asked: a model is set and the router
	// can build it. FallbackNote is the reason when one is set and it is not, a sentence
	// for a person; empty otherwise.
	FallbackActive bool
	FallbackNote   string
	ComposerModel  string
	EscalateBelow  float64
	MinConfidence  float64
	RetentionDays  int
	TrashToFolder  bool              // trash goes to actions.TrashFolder, not the server's Trash
	LeaveOwnMail   bool              // mail from the mailbox's own address is left alone (pipeline.OwnMail)
	OpenAIBaseURL  string            // an OpenAI-compatible endpoint; empty = api.openai.com
	OllamaURL      string            // a local Ollama server
	Keys           map[string]string // KeyStored | KeyEnvironment | KeyNone
	Warnings       []Warning         // never nil
	// The Claude workspace every Claude request names; empty = none. Its name is known when
	// a lookup listed it; Found says MailRules looked it up and stored it itself.
	AnthropicWorkspaceID    string
	AnthropicWorkspaceName  string
	AnthropicWorkspaceFound bool
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
	TrashToFolder *bool
	LeaveOwnMail  *bool
	OpenAIBaseURL *string // empty = api.openai.com
	OllamaURL     *string // empty = none
	// AnthropicWorkspaceID is empty or a wrkspc_ ID (config.ValidWorkspaceID); empty = none.
	AnthropicWorkspaceID *string
	Keys                 map[string]string
}

// Settings reads and changes the stored settings. Every setting belongs to a tenant: each
// method names the tenant it reads or changes, and a tenant with nothing stored has the
// environment's defaults.
//
// In MAILRULES_MODE=cloud the provider keys in the environment are the operator's: a
// tenant's calls use one only while that tenant has no key of its own for the provider,
// View never reports it (the key reads as not set, with no hint of where one is), and the
// calls made with it are booked with the operator mark (store.Ledger). In selfhost mode an
// environment key is the admin's own and is shown as set in the environment.
type Settings struct {
	Store  *store.Store
	Master []byte
	Env    *config.Config // the defaults
	Deps   models.Deps    // shared by every router built here
	// Workspaces looks up the workspace a Claude key needs; nil = never looked up, the
	// user types it.
	Workspaces WorkspaceFinder

	mu      sync.Mutex
	tenants map[int64]*routers // the routers built for each tenant, kept until its rows change

	// lookupMu keeps the workspace lookups, and changes to the key and the workspace,
	// one at a time, so a lookup for a key that is being replaced stores nothing stale.
	lookupMu sync.Mutex
}

// routers are the routers built for one tenant from its stored settings.
type routers struct {
	print     string        // the stored settings they were built from
	cfg       config.Config // the configuration in force when they were built
	router    *models.Router
	overrides map[string]*models.Router // per-rule model overrides, by decider spec; nil = cannot be built
	minCon    float64
}

// cloud reports whether environment keys are the operator's (MAILRULES_MODE=cloud).
func (s *Settings) cloud() bool { return s.Env.Mode == "cloud" }

// usageKeys are the keys a call to each provider (models.Usage.Provider) is made with.
var usageKeys = map[string][]string{
	"anthropic":  {"anthropic_api_key"},
	"openai":     {"openai_api_key"},
	"openrouter": {"openrouter_api_key"},
	"cloudflare": {"cloudflare_account_id", "cloudflare_api_token"},
}

// operatorKey reports whether the provider key name in force for a tenant, whose stored
// rows are rows, is the operator's: cloud mode, none stored by the tenant, one in the
// environment.
func (s *Settings) operatorKey(rows map[string]string, name string) bool {
	if !s.cloud() || keyFields[name] == nil || *keyFields[name](s.Env) == "" {
		return false
	}
	stored, ok := rows[keyPrefix+name]
	if ok {
		_, err := s.open(name, stored)
		ok = !errors.Is(err, crypto.ErrDecrypt) // one the master key cannot open is not set
	}
	return !ok
}

// operator says, for the tenant whose stored rows are rows, which providers' calls go out
// on an operator key.
func (s *Settings) operator(rows map[string]string) func(provider string) bool {
	return func(provider string) bool {
		for _, name := range usageKeys[provider] {
			if s.operatorKey(rows, name) {
				return true
			}
		}
		return false
	}
}

// ledger books a tenant's model calls, marking those made on an operator key.
func (s *Settings) ledger(tenantID int64, rows map[string]string) store.Ledger {
	return store.Ledger{Store: s.Store, TenantID: tenantID, Operator: s.operator(rows)}
}

// Ledger books a tenant's model calls made outside a router (the rule composer, a module),
// marking those made on an operator key as the tenant's settings stand now.
func (s *Settings) Ledger(ctx context.Context, tenantID int64) store.Ledger {
	rows, err := s.Store.Settings(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "could not read settings to book model usage", "error", err.Error())
		rows = map[string]string{}
	}
	return s.ledger(tenantID, rows)
}

// ErrNoComposer means no generative model is configured for the rule composer.
var ErrNoComposer = errors.New("no generative model is configured for the rule composer")

// ComposerMissing is ErrNoComposer with what the chosen composer model lacks: the one place
// that decides what the user is told to add.
type ComposerMissing struct {
	Model string // composer_model as set, e.g. openai:gpt-4o-mini
	Path  string // the setting to fill in, as the API spells it
	What  string // what to call it, e.g. "an OpenAI API key"
}

func (e *ComposerMissing) Error() string {
	return fmt.Sprintf("the rule composer model %s needs %s", e.Model, e.What)
}

// Is makes errors.Is(err, ErrNoComposer) true.
func (e *ComposerMissing) Is(target error) bool { return target == ErrNoComposer }

// Message is the sentence for a person: what the chosen model needs and where to add it.
func (e *ComposerMissing) Message() string {
	return fmt.Sprintf("This needs an AI model. The rule composer model, %s, needs %s: add it in Settings, then try again. Rules built from conditions work without one.", e.Model, e.What)
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

// own are the settings no environment variable sets: they have a built-in default only.
type own struct {
	retention     int
	trashToFolder bool
	leaveOwnMail  bool
}

// effective lays the stored rows over the environment.
func (s *Settings) effective(rows map[string]string) (config.Config, own, error) {
	cfg := *s.Env
	o := own{retention: DefaultRetentionDays, trashToFolder: store.DefaultTrashToFolder, leaveOwnMail: store.DefaultLeaveOwnMail}
	for key, dst := range map[string]any{
		"dry_run": &cfg.DryRun, "decider": &cfg.Decider, "decider_model": &cfg.DeciderModel,
		"fallback_model": &cfg.FallbackModel, "composer_model": &cfg.ComposerModel,
		"escalate_below": &cfg.EscalateBelow, "min_confidence": &cfg.MinConfidence, "retention_days": &o.retention,
		store.SettingTrashToFolder: &o.trashToFolder, store.SettingLeaveOwnMail: &o.leaveOwnMail,
		"openai_base_url": &cfg.OpenAIBaseURL, "ollama_url": &cfg.OllamaURL,
		settingWorkspace: &cfg.AnthropicWorkspaceID,
	} {
		if v, ok := rows[key]; ok {
			if err := json.Unmarshal([]byte(v), dst); err != nil {
				return cfg, own{}, fmt.Errorf("decode the %s setting: %w", key, err)
			}
		}
	}
	for name, field := range keyFields {
		if v, ok := rows[keyPrefix+name]; ok {
			plain, err := s.open(name, v)
			if errors.Is(err, crypto.ErrDecrypt) {
				// Sealed under a master key this install no longer has: as good as not set,
				// and it must not take every other setting down with it. Entering the key
				// again replaces it.
				continue
			}
			if err != nil {
				return cfg, own{}, err
			}
			*field(&cfg) = plain
		}
	}
	// A workspace MailRules found itself belongs to the key it was found for: with another
	// key (one set in the environment and changed since) it is not used.
	if l := readLookup(rows); l.Found != "" && cfg.AnthropicWorkspaceID == l.Found && l.Key != s.fingerprint(cfg.AnthropicAPIKey) {
		cfg.AnthropicWorkspaceID = s.Env.AnthropicWorkspaceID
	}
	// While an operator's key is in force (cloud mode, the tenant has none of its own), the
	// settings that say where that key is sent are the operator's too: a tenant's own base
	// URL would receive the operator's key in its Authorization header. The tenant's stored
	// values stay stored and take effect once it stores a key of its own.
	if s.operatorKey(rows, "openai_api_key") {
		cfg.OpenAIBaseURL = s.Env.OpenAIBaseURL
	}
	if s.operatorKey(rows, "anthropic_api_key") {
		cfg.AnthropicWorkspaceID = s.Env.AnthropicWorkspaceID
	}
	return cfg, o, nil
}

// Effective returns the tenant's configuration in force: the environment with the
// tenant's stored settings laid over it. It holds decrypted provider keys, so it is for
// building model adapters, never for showing.
func (s *Settings) Effective(ctx context.Context, tenantID int64) (config.Config, error) {
	rows, err := s.Store.Settings(ctx, tenantID)
	if err != nil {
		return config.Config{}, err
	}
	cfg, _, err := s.effective(rows)
	return cfg, err
}

func (s *Settings) view(rows map[string]string, cfg config.Config, o own) View {
	l := s.lookupFor(rows, cfg.AnthropicAPIKey)
	if s.operatorKey(rows, "anthropic_api_key") {
		l = workspaceLookup{} // learnt about the operator's key: not the tenant's to see
	}
	reason := models.FallbackSkipped(&cfg, cfg.DeciderSpec())
	v := View{DryRun: cfg.DryRun, Decider: cfg.Decider, DeciderModel: cfg.DeciderModel, FallbackModel: cfg.FallbackModel,
		FallbackActive: cfg.FallbackModel != "" && reason == models.FallbackNotSkipped, FallbackNote: fallbackNote(cfg.FallbackModel, reason),
		ComposerModel: cfg.ComposerModel, EscalateBelow: cfg.EscalateBelow, MinConfidence: cfg.MinConfidence,
		RetentionDays: o.retention, TrashToFolder: o.trashToFolder, LeaveOwnMail: o.leaveOwnMail,
		OpenAIBaseURL: cfg.OpenAIBaseURL, OllamaURL: cfg.OllamaURL,
		Keys: map[string]string{}, Warnings: append(warnings(cfg), workspaceWarning(cfg, l)...),
		AnthropicWorkspaceID: cfg.AnthropicWorkspaceID, AnthropicWorkspaceFound: l.Found != "" && l.Found == cfg.AnthropicWorkspaceID}
	// Under an operator's key the effective endpoint and workspace are the operator's; the
	// screen shows the tenant's own stored values instead (empty when none), never the
	// operator's.
	if s.operatorKey(rows, "anthropic_api_key") {
		v.AnthropicWorkspaceID = storedString(rows, settingWorkspace)
	}
	if s.operatorKey(rows, "openai_api_key") {
		v.OpenAIBaseURL = storedString(rows, "openai_base_url")
	}
	for _, w := range l.Workspaces {
		if w.ID == cfg.AnthropicWorkspaceID {
			v.AnthropicWorkspaceName = w.Name
		}
	}
	for name, field := range keyFields {
		stored, hasRow := rows[keyPrefix+name]
		if hasRow {
			// A stored key the master key cannot open is not set, as far as the screen goes.
			_, err := s.open(name, stored)
			hasRow = !errors.Is(err, crypto.ErrDecrypt)
		}
		switch {
		case hasRow:
			v.Keys[name] = KeyStored
		case *field(s.Env) != "" && !s.cloud(): // in cloud mode it is the operator's, never shown
			v.Keys[name] = KeyEnvironment
		default:
			v.Keys[name] = KeyNone
		}
	}
	return v
}

// storedString is a string setting as the tenant stored it, "" when it stored none.
func storedString(rows map[string]string, key string) string {
	var v string
	_ = json.Unmarshal([]byte(rows[key]), &v) // no row, or not a string: ""
	return v
}

// fallbackNote is the sentence that says why a configured fallback model is not asked, or
// "" when it is, or when none is set (the user turned it off).
func fallbackNote(model, reason string) string {
	switch reason {
	case models.FallbackNeedsKey:
		return fmt.Sprintf("Not active: %s needs a Claude (Anthropic) API key. Until one is set, an unsure decision is not double-checked.", model)
	case models.FallbackIsPrimary:
		return fmt.Sprintf("Not active: the decision model already is %s, so there is no second opinion to ask for.", model)
	}
	return ""
}

// providerNeeds is what each decider, and each provider the composer can use, cannot run
// without: the settings to fill in (as the API spells them) and what to call each.
// config.DeciderReady checks the same fields for the startup log;
// TestWarningsAgreeWithDeciderReady fails if the two lists part ways.
var providerNeeds = map[string][]struct {
	path, what string
	get        func(*config.Config) string
}{
	"jev":       {{"keys.openrouter_api_key", "an OpenRouter API key", func(c *config.Config) string { return c.OpenRouterAPIKey }}},
	"clef":      {{"keys.cloudflare_account_id", "a Cloudflare account ID", func(c *config.Config) string { return c.CloudflareAccountID }}, {"keys.cloudflare_api_token", "a Cloudflare API token", func(c *config.Config) string { return c.CloudflareAPIToken }}},
	"anthropic": {{"keys.anthropic_api_key", "a Claude (Anthropic) API key", func(c *config.Config) string { return c.AnthropicAPIKey }}},
	"openai":    {{"keys.openai_api_key", "an OpenAI API key", func(c *config.Config) string { return c.OpenAIAPIKey }}},
	"ollama":    {{"ollama_url", "the URL of your Ollama server", func(c *config.Config) string { return c.OllamaURL }}},
}

// composerExamples is a model for each provider the composer can use, to show in a refusal.
var composerExamples = map[string]string{"anthropic": "anthropic:claude-haiku-4-5", "openai": "openai:gpt-4o-mini", "ollama": "ollama:llama3.2"}

// warnings lists what the chosen decider and the rule composer's model still lack. A
// setting both lack is named once, by the decider's warning. It is never nil.
func warnings(cfg config.Config) []Warning {
	out := []Warning{}
	for _, n := range providerNeeds[cfg.Decider] {
		if n.get(&cfg) == "" {
			out = append(out, Warning{Code: "decider_not_ready", Path: n.path,
				Message: fmt.Sprintf("The %s decision model needs %s. Until it is set, rules that need a model are passed over.", cfg.Decider, n.what)})
		}
	}
	if m := composerMissing(cfg); m != nil && !slices.ContainsFunc(out, func(w Warning) bool { return w.Path == m.Path }) {
		out = append(out, Warning{Code: "composer_not_ready", Path: m.Path,
			Message: fmt.Sprintf("The rule composer model, %s, needs %s. Until it is set, Describe it, Rewrite with AI and Suggest from my mail do not work.", m.Model, m.What)})
	}
	return out
}

// composerMissing says what the composer's provider lacks first, or nil when it has
// everything or composer_model cannot be read (saving refuses such a value).
func composerMissing(cfg config.Config) *ComposerMissing {
	provider, _, err := config.SplitComposerModel(cfg.ComposerModel)
	if err != nil {
		return nil
	}
	for _, n := range providerNeeds[provider] {
		if n.get(&cfg) == "" {
			return &ComposerMissing{Model: cfg.ComposerModel, Path: n.path, What: n.what}
		}
	}
	return nil
}

// View returns the tenant's settings in force.
func (s *Settings) View(ctx context.Context, tenantID int64) (View, error) {
	rows, err := s.Store.Settings(ctx, tenantID)
	if err != nil {
		return View{}, err
	}
	cfg, o, err := s.effective(rows)
	if err != nil {
		return View{}, err
	}
	return s.view(rows, cfg, o), nil
}

// resettable are the settings Patch.Reset may name.
var resettable = []string{"dry_run", "decider", "decider_model", "fallback_model", "composer_model", "escalate_below",
	"min_confidence", "retention_days", store.SettingTrashToFolder, store.SettingLeaveOwnMail, "openai_base_url", "ollama_url",
	settingWorkspace}

// Apply validates a change against the settings it would produce and stores it in one
// transaction. A problem the user can fix comes back as *Invalid. Nothing needs a restart:
// the executor reads dry-run and trash_to_folder before every action, the pipeline reads
// leave_own_mail for every email, and Live rebuilds the router.
// A Claude key saved or replaced forgets what was learnt about the old one's workspace,
// and a workspace MailRules found for it; then, with no workspace set, it is looked up.
func (s *Settings) Apply(ctx context.Context, tenantID int64, p Patch) error {
	s.lookupMu.Lock()
	newKey, err := s.apply(ctx, tenantID, p)
	s.lookupMu.Unlock()
	if err == nil && newKey {
		if _, err := s.LookupWorkspaces(ctx, tenantID); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "could not look up the Anthropic workspace", "error", err.Error())
		}
	}
	return err
}

// apply is Apply under lookupMu. newKey reports that a Claude key was saved while no
// workspace is set, so its workspace is to be looked up.
func (s *Settings) apply(ctx context.Context, tenantID int64, p Patch) (newKey bool, err error) {
	rows, err := s.Store.Settings(ctx, tenantID)
	if err != nil {
		return false, err
	}
	before := readLookup(rows)
	storedWorkspace := rows[settingWorkspace]
	set := map[string]*string{}
	for _, name := range p.Reset {
		if !slices.Contains(resettable, name) {
			return false, &Invalid{name, "This setting cannot be reset."}
		}
		delete(rows, name) // the environment's default shows through
		set[name] = nil
	}
	cfg, o, err := s.effective(rows)
	if err != nil {
		return false, err
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
		o.retention = *p.RetentionDays
		put("retention_days", o.retention)
	}
	if p.TrashToFolder != nil {
		o.trashToFolder = *p.TrashToFolder
		put(store.SettingTrashToFolder, o.trashToFolder)
	}
	if p.LeaveOwnMail != nil {
		o.leaveOwnMail = *p.LeaveOwnMail
		put(store.SettingLeaveOwnMail, o.leaveOwnMail)
	}
	for key, f := range map[string]struct{ in, dst *string }{
		"openai_base_url": {p.OpenAIBaseURL, &cfg.OpenAIBaseURL}, "ollama_url": {p.OllamaURL, &cfg.OllamaURL},
	} {
		if f.in == nil {
			continue
		}
		*f.dst = strings.TrimSpace(*f.in)
		if u, err := url.Parse(*f.dst); *f.dst != "" && (err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "") {
			return false, &Invalid{key, "Enter an http or https URL, such as http://localhost:11434."}
		}
		put(key, *f.dst)
	}
	for name, value := range p.Keys {
		if keyFields[name] == nil {
			return false, &Invalid{"keys." + name, "Unknown key. The keys are " + strings.Join(KeyNames(), ", ") + "."}
		}
		if value = strings.TrimSpace(value); value == "" {
			set[keyPrefix+name] = nil
			continue
		}
		enc, err := s.seal(name, value)
		if err != nil {
			return false, fmt.Errorf("encrypt key %s: %w", name, err)
		}
		set[keyPrefix+name] = &enc
	}
	if err := s.applyWorkspace(p, &cfg, before, storedWorkspace, set); err != nil {
		return false, err
	}
	newKey = strings.TrimSpace(p.Keys["anthropic_api_key"]) != "" && cfg.AnthropicWorkspaceID == ""

	// The same rules as config.Validate, worded for a form field.
	switch {
	case !slices.Contains(Deciders, cfg.Decider):
		return false, &Invalid{"decider", "The decision model must be one of " + strings.Join(Deciders, ", ") + "."}
	case (cfg.Decider == "openai" || cfg.Decider == "ollama") && cfg.DeciderModel == "":
		return false, &Invalid{"decider_model", "The " + cfg.Decider + " decider has no default model. Name one."}
	case cfg.ComposerModel == "":
		return false, &Invalid{"composer_model", "The rule composer needs a model."}
	}
	if provider, _, err := config.SplitComposerModel(cfg.ComposerModel); err != nil {
		if example, known := composerExamples[provider]; known {
			return false, &Invalid{"composer_model", fmt.Sprintf("Name the model after %s:, for example %s.", provider, example)}
		}
		return false, &Invalid{"composer_model", "The rule composer model must be a Claude model name, such as claude-haiku-4-5, or start with anthropic:, openai: or ollama: and then name the model."}
	}
	switch {
	case cfg.EscalateBelow < 0 || cfg.EscalateBelow > 1:
		return false, &Invalid{"escalate_below", "The escalation threshold must be between 0 and 1."}
	case cfg.MinConfidence < 0 || cfg.MinConfidence > 1:
		return false, &Invalid{"min_confidence", "The confidence threshold must be between 0 and 1."}
	case o.retention < 1 || o.retention > maxRetentionDays:
		return false, &Invalid{"retention_days", fmt.Sprintf("Retention must be between 1 and %d days.", maxRetentionDays)}
	}
	if err := cfg.Validate(); err != nil { // whatever config.Validate learns to check later
		return false, &Invalid{"", "These settings cannot be used together: " + err.Error() + "."}
	}
	return newKey, s.Store.SetSettings(ctx, tenantID, set)
}

// Live returns the tenant's decision router and act threshold for its settings as stored
// right now; pipeline.Pipeline.Live is this method. The router is rebuilt only when one
// of the tenant's stored settings has changed since the last call, whoever changed it, so
// a save in the browser reaches the next email without a restart. router is nil while the
// decider lacks its key.
func (s *Settings) Live(ctx context.Context, tenantID int64) (router *models.Router, minConfidence float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, _ := s.live(ctx, tenantID)
	if t == nil {
		return nil, s.Env.MinConfidence
	}
	return t.router, t.minCon
}

// live is Live under mu: the tenant's routers, rebuilt when its rows changed. It is nil
// when the settings cannot be read and nothing was built before.
func (s *Settings) live(ctx context.Context, tenantID int64) (*routers, map[string]string) {
	t := s.tenants[tenantID]
	rows, err := s.Store.Settings(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "could not read settings; keeping the last ones", "error", err.Error())
		return t, nil
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
	if t != nil && b.String() == t.print {
		return t, rows
	}
	cfg, _, err := s.effective(rows)
	if err != nil {
		slog.WarnContext(ctx, "stored settings cannot be used; mail that needs a model waits in Needs review", "error", err.Error())
		cfg = *s.Env
	}
	t = &routers{print: b.String(), cfg: cfg, overrides: map[string]*models.Router{}, minCon: cfg.MinConfidence}
	if ready := cfg.DeciderReady(); ready != nil {
		slog.WarnContext(ctx, "no decision model yet; new mail that needs one waits in Needs review until it is set", "tenant", tenantID, "missing", ready.Error())
	} else if t.router, err = models.NewRouter(&cfg, cfg.DeciderSpec(), s.deps(tenantID), s.ledger(tenantID, rows)); err != nil {
		slog.WarnContext(ctx, "could not set up the decision model", "tenant", tenantID, "error", err.Error())
		t.router = nil
	}
	if t.router != nil {
		// One line each time the settings change, not per email: this runs only on a rebuild.
		if note := fallbackNote(cfg.FallbackModel, models.FallbackSkipped(&cfg, cfg.DeciderSpec())); note != "" {
			slog.WarnContext(ctx, "fallback model is not active, so low-confidence decisions are not double-checked", "tenant", tenantID, "fallback_model", cfg.FallbackModel, "reason", strings.TrimPrefix(note, "Not active: "))
		}
	}
	if s.tenants == nil {
		s.tenants = map[int64]*routers{}
	}
	s.tenants[tenantID] = t
	return t, rows
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

// RouterFor returns the tenant's router for a rule's model override (a decider spec, see
// CheckModel), built from the tenant's settings in force and kept until they change. It is
// nil when that decider cannot be used, for example because its key is not set; the caller
// then decides with the default router. pipeline.Pipeline.Override is this method.
func (s *Settings) RouterFor(ctx context.Context, tenantID int64, spec string) *models.Router {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, rows := s.live(ctx, tenantID) // rebuilds, and forgets the overrides, when a stored setting changed
	if t == nil {
		return nil
	}
	if r, ok := t.overrides[spec]; ok {
		return r
	}
	if rows == nil { // the settings could not be read just now: book as the last build did
		rows = map[string]string{}
	}
	r, err := models.NewRouter(&t.cfg, spec, s.deps(tenantID), s.ledger(tenantID, rows))
	if err != nil {
		slog.WarnContext(ctx, "a rule's model cannot be used; the default decides instead", "model", spec, "error", err.Error())
		r = nil
	}
	t.overrides[spec] = r
	return r
}

// Composer returns the generative model the tenant's rule composer writes rules with:
// composer_model as config.SplitComposerModel reads it, on Anthropic, an
// OpenAI-compatible endpoint or Ollama. Until that provider has its key or URL it is a
// *ComposerMissing, which is ErrNoComposer. Its calls are booked by the caller, through
// Ledger.
func (s *Settings) Composer(ctx context.Context, tenantID int64) (models.Generator, error) {
	cfg, err := s.Effective(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if m := composerMissing(cfg); m != nil {
		return nil, m
	}
	gen, err := models.NewGenerator(&cfg, cfg.ComposerModel, s.deps(tenantID))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoComposer, err)
	}
	return gen, nil
}

// RetentionDays returns the tenant's retention_days setting in force: how long message
// snippets are kept. When the settings cannot be read it is the default.
func (s *Settings) RetentionDays(ctx context.Context, tenantID int64) int {
	v, err := s.View(ctx, tenantID)
	if err != nil {
		slog.WarnContext(ctx, "could not read the retention setting; using the default", "error", err.Error())
		return DefaultRetentionDays
	}
	return v.RetentionDays
}
