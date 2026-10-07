package models

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
)

// The composer's model is built from the same provider:model spelling as the decider's.
func TestNewGenerator(t *testing.T) {
	cfg := &config.Config{AnthropicAPIKey: "k", OpenAIAPIKey: "k", OllamaURL: "http://localhost:11434"}
	tests := []struct {
		spec, want string // want: the adapter's type, or a substring of the error
	}{
		{"claude-haiku-4-5", "*models.Anthropic"},
		{"anthropic:claude-haiku-4-5", "*models.Anthropic"},
		{"openai:gpt-4o-mini", "*models.OpenAI"},
		{"ollama:llama3.2", "*models.Ollama"},
		{"openai:", "composer openai needs a model: write it as openai:<model>"},
		{"foo:bar", `unknown composer provider "foo"`},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			gen, err := NewGenerator(cfg, tt.spec, testDeps())
			if err != nil {
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("err = %v, want %q", err, tt.want)
				}
				return
			}
			if got := fmt.Sprintf("%T", gen); got != tt.want {
				t.Errorf("NewGenerator = %s, want %s", got, tt.want)
			}
		})
	}
	// The decider says the same thing in the same words.
	if _, err := NewDecider(&config.Config{OpenAIAPIKey: "k"}, "openai", testDeps()); err == nil || err.Error() != "decider openai needs a model: write it as openai:<model>" {
		t.Errorf("NewDecider(openai) = %v", err)
	}
}
