package config

import (
	"strings"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || !c.DryRun || c.Decider != "jev" || c.EscalateBelow != 0.75 || c.BodyChars != 2000 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadPrecedence(t *testing.T) {
	c, err := Load([]string{"--listen", "127.0.0.1:9000"}, env(map[string]string{
		"MAILRULES_LISTEN":  "127.0.0.1:7000",
		"MAILRULES_DRY_RUN": "false",
		"OLLAMA_URL":        "http://127.0.0.1:11434",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" {
		t.Errorf("flag should beat env, got %s", c.Listen)
	}
	if c.DryRun {
		t.Error("MAILRULES_DRY_RUN=false ignored")
	}
	if c.OllamaURL == "" {
		t.Error("OLLAMA_URL ignored")
	}
}

func TestLoadBadEnv(t *testing.T) {
	_, err := Load(nil, env(map[string]string{"MAILRULES_BODY_CHARS": "lots", "MAILRULES_DRY_RUN": "maybe"}))
	if err == nil || !strings.Contains(err.Error(), "MAILRULES_BODY_CHARS") || !strings.Contains(err.Error(), "MAILRULES_DRY_RUN") {
		t.Fatalf("want both env names in error, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string // substrings of the error; empty means valid
	}{
		{"defaults need no key", nil, nil},
		{"unknown decider", map[string]string{"MAILRULES_DECIDER": "gpt"}, []string{`MAILRULES_DECIDER="gpt"`}},
		{"bad mode", map[string]string{"MAILRULES_MODE": "saas"}, []string{"MAILRULES_MODE"}},
		{"bad listen", map[string]string{"MAILRULES_LISTEN": "8080"}, []string{"MAILRULES_LISTEN"}},
		{"threshold out of range", map[string]string{"MAILRULES_ESCALATE_BELOW": "1.5", "MAILRULES_MIN_CONFIDENCE": "-1"}, []string{"MAILRULES_ESCALATE_BELOW", "MAILRULES_MIN_CONFIDENCE"}},
		{"non-positive numbers", map[string]string{"MAILRULES_BODY_CHARS": "0", "MAILRULES_MODEL_CONCURRENCY": "-2"}, []string{"MAILRULES_BODY_CHARS", "MAILRULES_MODEL_CONCURRENCY"}},
		{"both master keys", map[string]string{"MAILRULES_MASTER_KEY": "a", "MAILRULES_MASTER_KEY_FILE": "b"}, []string{"only one of"}},
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, []string{"LOG_LEVEL"}},
		{"empty fallback disables escalation", map[string]string{"MAILRULES_FALLBACK_MODEL": ""}, nil},
		{"openai needs a model", map[string]string{"MAILRULES_DECIDER": "openai"}, []string{"MAILRULES_DECIDER=openai needs MAILRULES_DECIDER_MODEL"}},
		{"ollama needs a model", map[string]string{"MAILRULES_DECIDER": "ollama"}, []string{"MAILRULES_DECIDER=ollama needs MAILRULES_DECIDER_MODEL"}},
		{"ollama with a model", map[string]string{"MAILRULES_DECIDER": "ollama", "MAILRULES_DECIDER_MODEL": "llama3.2"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(nil, env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			err = c.Validate()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("want valid, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestDeciderReady(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"jev with key", map[string]string{"OPENROUTER_API_KEY": "k"}, nil},
		{"jev without key", nil, []string{"MAILRULES_DECIDER=jev but OPENROUTER_API_KEY is empty"}},
		{"clef missing both", map[string]string{"MAILRULES_DECIDER": "clef"}, []string{"CLOUDFLARE_ACCOUNT_ID is empty", "CLOUDFLARE_API_TOKEN is empty"}},
		{"anthropic ok", map[string]string{"MAILRULES_DECIDER": "anthropic", "ANTHROPIC_API_KEY": "k"}, nil},
		{"openai missing key", map[string]string{"MAILRULES_DECIDER": "openai"}, []string{"OPENAI_API_KEY is empty"}},
		{"ollama ok", map[string]string{"MAILRULES_DECIDER": "ollama", "OLLAMA_URL": "http://x"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(nil, env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			err = c.DeciderReady()
			if len(tt.want) == 0 {
				if err != nil {
					t.Fatalf("want ready, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want error, got nil")
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q missing %q", err, w)
				}
			}
		})
	}
}

func TestSecureCookies(t *testing.T) {
	tests := []struct {
		listen, setting string
		want            bool
	}{
		{"127.0.0.1:8080", "auto", false},
		{"0.0.0.0:8080", "auto", true},
		{"0.0.0.0:8080", "false", false},
		{"127.0.0.1:8080", "true", true},
	}
	for _, tt := range tests {
		c := &Config{Listen: tt.listen, CookieSecure: tt.setting}
		if got := c.SecureCookies(); got != tt.want {
			t.Errorf("listen %s, setting %s: got %v want %v", tt.listen, tt.setting, got, tt.want)
		}
	}
	c, err := Load(nil, env(map[string]string{"MAILRULES_COOKIE_SECURE": "sometimes"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "MAILRULES_COOKIE_SECURE") {
		t.Errorf("bad value accepted: %v", err)
	}
}

func TestListensLocally(t *testing.T) {
	for addr, want := range map[string]bool{"127.0.0.1:8080": true, "localhost:8080": true, "[::1]:8080": true, "0.0.0.0:8080": false, ":8080": false, "192.168.1.4:80": false} {
		if got := (&Config{Listen: addr}).ListensLocally(); got != want {
			t.Errorf("%s: got %v want %v", addr, got, want)
		}
	}
}

func TestDeciderSpec(t *testing.T) {
	c, _ := Load([]string{"--decider-model", "clef-flash"}, env(map[string]string{"MAILRULES_DECIDER": "clef"}))
	if got := c.DeciderSpec(); got != "clef:clef-flash" {
		t.Errorf("spec = %q", got)
	}
	if c, _ = Load(nil, env(nil)); c.DeciderSpec() != "jev" {
		t.Errorf("default spec = %q", c.DeciderSpec())
	}
}
