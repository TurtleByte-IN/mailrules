package settings

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// newSettings is Settings over a fresh database, with env as the daemon's environment.
func newSettings(t *testing.T, env map[string]string) *Settings {
	t.Helper()
	ctx := t.Context()
	db, err := store.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(nil, func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	return &Settings{Store: store.New(db), Master: make([]byte, 32), Env: cfg, Deps: models.Deps{Caller: models.NewCaller(1), Prices: models.DefaultPrices()}}
}

// composer_model is a Claude model, or provider:model; anything else is refused on its field.
func TestComposerModelIsCheckedOnSave(t *testing.T) {
	tests := []struct {
		value, refusal string // refusal "" = saved
	}{
		{"claude-sonnet-4-5", ""},
		{"anthropic:claude-haiku-4-5", ""},
		{"openai:gpt-4o-mini", ""},
		{"ollama:llama3.2", ""},
		{"  ollama:llama3.2:3b  ", ""},
		{"openai:", "Name the model after openai:, for example openai:gpt-4o-mini."},
		{"ollama:", "Name the model after ollama:, for example ollama:llama3.2."},
		{"anthropic:", "Name the model after anthropic:, for example anthropic:claude-haiku-4-5."},
		{"foo:bar", "The rule composer model must be a Claude model name, such as claude-haiku-4-5, or start with anthropic:, openai: or ollama: and then name the model."},
		{"", "The rule composer needs a model."},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			s := newSettings(t, nil)
			err := s.Apply(t.Context(), 1, Patch{ComposerModel: &tt.value})
			var inv *Invalid
			switch {
			case tt.refusal == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tt.refusal != "" && (!errors.As(err, &inv) || inv.Path != "composer_model" || inv.Message != tt.refusal):
				t.Fatalf("Apply = %v, want composer_model: %s", err, tt.refusal)
			}
			v, err := s.View(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.TrimSpace(tt.value)
			if tt.refusal != "" {
				want = "claude-haiku-4-5" // nothing was stored: the environment's default is in force
			}
			if v.ComposerModel != want {
				t.Errorf("composer_model in force = %q, want %q", v.ComposerModel, want)
			}
		})
	}
}

// Composer builds the chosen provider's adapter, or says what that provider lacks.
func TestComposerPerProvider(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		wantType string // the adapter, when it can be built
		path     string // what is missing, when it cannot
		message  string
	}{
		{"a stored bare name stays Claude", map[string]string{"ANTHROPIC_API_KEY": "k"}, "*models.Anthropic", "", ""},
		{"anthropic: is Claude", map[string]string{"MAILRULES_COMPOSER_MODEL": "anthropic:claude-haiku-4-5", "ANTHROPIC_API_KEY": "k"}, "*models.Anthropic", "", ""},
		{"Claude without its key", nil, "", "keys.anthropic_api_key",
			"This needs an AI model. The rule composer model, claude-haiku-4-5, needs a Claude (Anthropic) API key: add it in Settings, then try again. Rules built from conditions work without one."},
		{"openai with its key", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:gpt-4o-mini", "OPENAI_API_KEY": "k"}, "*models.OpenAI", "", ""},
		{"openai with only a Claude key", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:gpt-4o-mini", "ANTHROPIC_API_KEY": "k"}, "", "keys.openai_api_key",
			"This needs an AI model. The rule composer model, openai:gpt-4o-mini, needs an OpenAI API key: add it in Settings, then try again. Rules built from conditions work without one."},
		{"ollama with its URL, no key at all", map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:llama3.2", "OLLAMA_URL": "http://localhost:11434"}, "*models.Ollama", "", ""},
		{"ollama without its URL", map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:llama3.2", "ANTHROPIC_API_KEY": "k"}, "", "ollama_url",
			"This needs an AI model. The rule composer model, ollama:llama3.2, needs the URL of your Ollama server: add it in Settings, then try again. Rules built from conditions work without one."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gen, err := newSettings(t, tt.env).Composer(t.Context(), 1)
			if tt.wantType != "" {
				if err != nil || fmt.Sprintf("%T", gen) != tt.wantType {
					t.Fatalf("Composer() = %T, %v; want %s", gen, err, tt.wantType)
				}
				return
			}
			var missing *ComposerMissing
			if !errors.Is(err, ErrNoComposer) || !errors.As(err, &missing) || missing.Path != tt.path || missing.Message() != tt.message {
				t.Fatalf("Composer() = %v, %v; want ErrNoComposer for %s:\n%s", gen, err, tt.path, tt.message)
			}
		})
	}
}

// Settings warn when the composer's provider lacks its key or URL, once per setting.
func TestComposerWarnings(t *testing.T) {
	tests := []struct {
		name string
		cfg  config.Config
		want string // code@path of each warning
	}{
		{"ready", config.Config{Decider: "ollama", OllamaURL: "http://o", ComposerModel: "ollama:llama3.2"}, ""},
		{"openai composer without its key", config.Config{Decider: "jev", OpenRouterAPIKey: "k", ComposerModel: "openai:gpt-4o-mini"}, "composer_not_ready@keys.openai_api_key "},
		{"both lack something", config.Config{Decider: "jev", ComposerModel: "ollama:llama3.2"}, "decider_not_ready@keys.openrouter_api_key composer_not_ready@ollama_url "},
		{"the same key, named once", config.Config{Decider: "anthropic", ComposerModel: "claude-haiku-4-5"}, "decider_not_ready@keys.anthropic_api_key "},
		{"an unreadable value is the save's business", config.Config{Decider: "jev", OpenRouterAPIKey: "k", ComposerModel: "foo:bar"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got string
			for _, w := range warnings(tt.cfg) {
				got += w.Code + "@" + w.Path + " "
			}
			if got != tt.want {
				t.Errorf("warnings = %q, want %q", got, tt.want)
			}
		})
	}
	ws := warnings(config.Config{Decider: "jev", OpenRouterAPIKey: "k", ComposerModel: "openai:gpt-4o-mini"})
	if want := "The rule composer model, openai:gpt-4o-mini, needs an OpenAI API key. Until it is set, Describe it, Rewrite with AI and Suggest from my mail do not work."; ws[0].Message != want {
		t.Errorf("message = %q", ws[0].Message)
	}
}
