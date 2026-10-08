package config

import (
	"os"
	"path/filepath"
	"slices"
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

// The settings reference is generated from Settings, so every entry must say where it
// belongs and what it does, and no two may share a name.
func TestSettingsDescribed(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range Settings() {
		if s.Group == "" || s.Help == "" || s.Flag != flagName(s.Env) {
			t.Errorf("%s is not fully described: %+v", s.Env, s)
		}
		if seen[s.Env] {
			t.Errorf("%s is listed twice", s.Env)
		}
		seen[s.Env] = true
	}
	if len(seen) == 0 {
		t.Fatal("no settings listed")
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
		{"composer on openai", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:gpt-4o-mini"}, nil},
		{"composer provider without a model", map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:"}, []string{`MAILRULES_COMPOSER_MODEL="ollama:"`, "composer ollama needs a model: write it as ollama:<model>"}},
		{"composer on an unknown provider", map[string]string{"MAILRULES_COMPOSER_MODEL": "gemini:pro"}, []string{`unknown composer provider "gemini"`}},
		{"a Claude workspace", map[string]string{"ANTHROPIC_WORKSPACE_ID": "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ"}, nil},
		{"not a Claude workspace", map[string]string{"ANTHROPIC_WORKSPACE_ID": "default"}, []string{`ANTHROPIC_WORKSPACE_ID="default" must start with wrkspc_`}},
		{"a full mail server", map[string]string{"MAILRULES_SMTP_HOST": "smtp.fastmail.com", "MAILRULES_SMTP_PORT": "465", "MAILRULES_SMTP_TLS": "implicit",
			"MAILRULES_SMTP_USER": "me@fastmail.com", "MAILRULES_SMTP_PASSWORD": "secret", "MAILRULES_SMTP_FROM": "MailRules <me@fastmail.com>"}, nil},
		{"no TLS on this machine", map[string]string{"MAILRULES_SMTP_HOST": "localhost", "MAILRULES_SMTP_TLS": "none"}, nil},
		{"no TLS on loopback", map[string]string{"MAILRULES_SMTP_HOST": "::1", "MAILRULES_SMTP_TLS": "none"}, nil},
		{"no TLS elsewhere", map[string]string{"MAILRULES_SMTP_HOST": "smtp.example.com", "MAILRULES_SMTP_TLS": "none"}, []string{"MAILRULES_SMTP_TLS=none", "only for a mail server on this machine"}},
		{"unknown TLS mode", map[string]string{"MAILRULES_SMTP_TLS": "ssl"}, []string{`MAILRULES_SMTP_TLS="ssl" must be starttls, implicit or none`}},
		{"host with a port", map[string]string{"MAILRULES_SMTP_HOST": "smtp.example.com:587"}, []string{"MAILRULES_SMTP_HOST", "without a scheme or port"}},
		{"host as a URL", map[string]string{"MAILRULES_SMTP_HOST": "smtp://smtp.example.com"}, []string{"MAILRULES_SMTP_HOST"}},
		{"port out of range", map[string]string{"MAILRULES_SMTP_PORT": "70000"}, []string{"MAILRULES_SMTP_PORT=70000"}},
		{"both SMTP passwords", map[string]string{"MAILRULES_SMTP_PASSWORD": "a", "MAILRULES_SMTP_PASSWORD_FILE": "b"}, []string{"only one of MAILRULES_SMTP_PASSWORD"}},
		{"bad From", map[string]string{"MAILRULES_SMTP_FROM": "not an address"}, []string{"MAILRULES_SMTP_FROM"}},
		{"two From addresses", map[string]string{"MAILRULES_SMTP_FROM": "a@example.com, b@example.com"}, []string{"MAILRULES_SMTP_FROM"}},
		{"trusted proxies", map[string]string{"MAILRULES_TRUSTED_PROXIES": "127.0.0.1, 172.18.0.0/16 ::1,fd00::/8"}, nil},
		{"trusted proxy that is not an address", map[string]string{"MAILRULES_TRUSTED_PROXIES": "caddy"}, []string{"MAILRULES_TRUSTED_PROXIES", `"caddy"`}},
		{"trusted proxy with a bad range", map[string]string{"MAILRULES_TRUSTED_PROXIES": "10.0.0.0/40"}, []string{`"10.0.0.0/40"`}},
		{"trusting everyone", map[string]string{"MAILRULES_TRUSTED_PROXIES": "0.0.0.0/0"}, []string{"would trust every address"}},
		{"public URL behind a proxy", map[string]string{"MAILRULES_PUBLIC_URL": "https://mail.example.com/mailrules/"}, nil},
		{"public URL without a scheme", map[string]string{"MAILRULES_PUBLIC_URL": "mail.example.com"}, []string{"MAILRULES_PUBLIC_URL"}},
		{"public URL with a hash", map[string]string{"MAILRULES_PUBLIC_URL": "https://mail.example.com/#/"}, []string{"MAILRULES_PUBLIC_URL"}},
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

func TestSMTPMissing(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{"nothing set", nil, []string{"MAILRULES_SMTP_HOST", "MAILRULES_SMTP_FROM"}},
		{"no sign-in", map[string]string{"MAILRULES_SMTP_HOST": "localhost", "MAILRULES_SMTP_FROM": "m@example.com"}, []string{}},
		{"user without password", map[string]string{"MAILRULES_SMTP_HOST": "h", "MAILRULES_SMTP_FROM": "m@example.com", "MAILRULES_SMTP_USER": "u"}, []string{"MAILRULES_SMTP_PASSWORD"}},
		{"password file without user", map[string]string{"MAILRULES_SMTP_HOST": "h", "MAILRULES_SMTP_FROM": "m@example.com", "MAILRULES_SMTP_PASSWORD_FILE": "/run/secrets/smtp"}, []string{"MAILRULES_SMTP_USER"}},
		{"everything", map[string]string{"MAILRULES_SMTP_HOST": "h", "MAILRULES_SMTP_FROM": "m@example.com", "MAILRULES_SMTP_USER": "u", "MAILRULES_SMTP_PASSWORD": "p"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(nil, env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			if got := c.SMTPMissing(); !slices.Equal(got, tt.want) {
				t.Errorf("SMTPMissing() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSMTPAddr(t *testing.T) {
	for _, tt := range []struct {
		tls  string
		port int
		want string
	}{{"starttls", 0, "h:587"}, {"implicit", 0, "h:465"}, {"none", 0, "h:25"}, {"starttls", 2525, "h:2525"}} {
		c := Config{SMTPHost: "h", SMTPTLS: tt.tls, SMTPPort: tt.port}
		if got := c.SMTPAddr(); got != tt.want {
			t.Errorf("%s port %d: SMTPAddr() = %q, want %q", tt.tls, tt.port, got, tt.want)
		}
	}
}

func TestSMTPSecret(t *testing.T) {
	file := filepath.Join(t.TempDir(), "smtp")
	if err := os.WriteFile(file, []byte("pass word\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (&Config{SMTPPasswordFile: file}).SMTPSecret(); err != nil || got != "pass word" {
		t.Errorf("from the file: %q, %v; want the password without its line ending", got, err)
	}
	if got, err := (&Config{SMTPPassword: "direct"}).SMTPSecret(); err != nil || got != "direct" {
		t.Errorf("from the variable: %q, %v", got, err)
	}
	if _, err := (&Config{SMTPPasswordFile: filepath.Join(t.TempDir(), "absent")}).SMTPSecret(); err == nil || !strings.Contains(err.Error(), "MAILRULES_SMTP_PASSWORD_FILE") {
		t.Errorf("a missing file: %v, want an error naming the setting", err)
	}
}

func TestValidWorkspaceID(t *testing.T) {
	tests := []struct {
		id   string
		want bool
	}{
		{"", true},
		{"wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ", true},
		{"wrkspc_", false},
		{"default", false},
		{"WRKSPC_01Jw", false},
		{" wrkspc_01Jw", false},
		{"wrkspc_01-Jw", false},
		{"wrkspc_01Jw\r\nX-Other: 1", false},
	}
	for _, tt := range tests {
		if got := ValidWorkspaceID(tt.id); got != tt.want {
			t.Errorf("ValidWorkspaceID(%q) = %v, want %v", tt.id, got, tt.want)
		}
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

// composer_model is a bare Claude model, as it always was, or provider:model.
func TestSplitComposerModel(t *testing.T) {
	tests := []struct {
		spec, provider, model, err string // err: a substring; "" means it is read
	}{
		{"claude-haiku-4-5", "anthropic", "claude-haiku-4-5", ""},
		{"anthropic:claude-sonnet-4-5", "anthropic", "claude-sonnet-4-5", ""},
		{"openai:gpt-4o-mini", "openai", "gpt-4o-mini", ""},
		{"ollama:llama3.2", "ollama", "llama3.2", ""},
		{"ollama:llama3.2:3b", "ollama", "llama3.2:3b", ""}, // the model keeps its own colon
		{"openai:", "openai", "", "composer openai needs a model: write it as openai:<model>"},
		{"anthropic:", "anthropic", "", "composer anthropic needs a model"},
		{"foo:bar", "foo", "bar", `unknown composer provider "foo": must be anthropic, openai, ollama`},
		{"", "", "", "the rule composer needs a model"},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			provider, model, err := SplitComposerModel(tt.spec)
			if provider != tt.provider || model != tt.model {
				t.Errorf("= %q, %q; want %q, %q", provider, model, tt.provider, tt.model)
			}
			if tt.err == "" && err != nil || tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("err = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestComposerReady(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string // a substring of the error; "" means ready
	}{
		{"default Claude without a key", nil, "MAILRULES_COMPOSER_MODEL=claude-haiku-4-5 but ANTHROPIC_API_KEY is empty"},
		{"default Claude with a key", map[string]string{"ANTHROPIC_API_KEY": "k"}, ""},
		{"openai without a key", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:gpt-4o-mini", "ANTHROPIC_API_KEY": "k"}, "OPENAI_API_KEY is empty"},
		{"openai with a key", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:gpt-4o-mini", "OPENAI_API_KEY": "k"}, ""},
		{"ollama without a URL", map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:llama3.2"}, "OLLAMA_URL is empty"},
		{"ollama with a URL", map[string]string{"MAILRULES_COMPOSER_MODEL": "ollama:llama3.2", "OLLAMA_URL": "http://x"}, ""},
		{"no model", map[string]string{"MAILRULES_COMPOSER_MODEL": "openai:"}, "needs a model"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(nil, env(tt.env))
			if err != nil {
				t.Fatal(err)
			}
			err = c.ComposerReady()
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("ComposerReady() = %v, want %q", err, tt.want)
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

func TestTrustedProxyPrefixes(t *testing.T) {
	c := &Config{TrustedProxies: " 127.0.0.1,172.18.5.9/16\t::1 "}
	got, err := c.TrustedProxyPrefixes()
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, p := range got {
		s = append(s, p.String())
	}
	if want := "127.0.0.1/32 172.18.0.0/16 ::1/128"; strings.Join(s, " ") != want {
		t.Errorf("prefixes = %v, want %s", s, want)
	}
	if got, err := (&Config{}).TrustedProxyPrefixes(); err != nil || len(got) != 0 {
		t.Errorf("empty setting = %v, %v; want no proxies", got, err)
	}
}
