package settings

import (
	"bytes"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// sealedStore holds that many mailbox passwords sealed under key(1), and a provider key
// for each entry of keys, sealed under the key it maps to.
func sealedStore(t *testing.T, accounts int, keys map[string][]byte) *store.Store {
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
	st := store.New(db)
	u, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	for range accounts {
		if _, err := st.CreateAccount(ctx, key(1), store.Account{UserID: u.ID, Label: "a", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "u", CreatedAt: 1}, "app password"); err != nil {
			t.Fatal(err)
		}
	}
	for name, k := range keys {
		enc, err := (&Settings{Master: k}).seal(name, "sk-secret")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetSetting(ctx, keyPrefix+name, enc); err != nil {
			t.Fatal(err)
		}
	}
	// Settings that are not secrets are not counted.
	if err := st.SetSetting(ctx, "dry_run", "true"); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestCheckSecrets(t *testing.T) {
	tests := []struct {
		name     string
		accounts int
		keys     map[string][]byte
		master   []byte
		want     SecretsCheck
	}{
		{"nothing stored", 0, nil, key(1), SecretsCheck{}},
		{"nothing stored, no key yet", 0, nil, nil, SecretsCheck{}},
		{"one mailbox, right key", 1, nil, key(1), SecretsCheck{Total: 1}},
		{"one mailbox, wrong key", 1, nil, key(9), SecretsCheck{Total: 1, Unreadable: 1}},
		{"one mailbox, no key yet", 1, nil, nil, SecretsCheck{Total: 1, Unreadable: 1}},
		{"only a provider key, wrong key", 0, map[string][]byte{"openrouter": key(1)}, key(9), SecretsCheck{Total: 1, Unreadable: 1}},
		{"only a provider key, no key yet", 0, map[string][]byte{"openrouter": key(1)}, nil, SecretsCheck{Total: 1, Unreadable: 1}},
		{"mailboxes and keys, all readable", 2, map[string][]byte{"openrouter": key(1), "anthropic": key(1)}, key(1), SecretsCheck{Total: 4}},
		{"half of them sealed under another key", 2, map[string][]byte{"openrouter": key(1), "anthropic": key(2)}, key(1), SecretsCheck{Total: 4, Unreadable: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := sealedStore(t, tt.accounts, tt.keys)
			got, err := CheckSecrets(t.Context(), st, tt.master)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("CheckSecrets = %+v, want %+v", got, tt.want)
			}
			if got.Readable() != tt.want.Total-tt.want.Unreadable {
				t.Errorf("Readable = %d", got.Readable())
			}
		})
	}
}

// A stored key whose value is damaged is one the master key does not protect, not a failure
// of the check.
func TestCheckSecretsDamagedRow(t *testing.T) {
	st := sealedStore(t, 0, nil)
	if err := st.SetSetting(t.Context(), keyPrefix+"openrouter", "not json"); err != nil {
		t.Fatal(err)
	}
	got, err := CheckSecrets(t.Context(), st, key(1))
	if err != nil || got != (SecretsCheck{Total: 1, Unreadable: 1}) {
		t.Fatalf("CheckSecrets = %+v, %v", got, err)
	}
}

// A provider key sealed under a master key that is gone does not take the other settings
// down: it reads as not set, the screen says so, and entering the key again replaces it.
func TestUnreadableStoredKeyIsNotSet(t *testing.T) {
	ctx := t.Context()
	st := sealedStore(t, 0, map[string][]byte{"openrouter_api_key": key(1), "anthropic_api_key": key(2)})
	env, err := config.Load(nil, func(k string) string { return map[string]string{"OPENAI_API_KEY": "sk-from-env"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{Store: st, Master: key(2), Env: env}

	cfg, err := s.Effective(ctx)
	if err != nil {
		t.Fatalf("Effective with one unreadable key: %v", err)
	}
	if cfg.OpenRouterAPIKey != "" || cfg.AnthropicAPIKey != "sk-secret" || cfg.OpenAIAPIKey != "sk-from-env" {
		t.Errorf("keys in force = %q %q %q", cfg.OpenRouterAPIKey, cfg.AnthropicAPIKey, cfg.OpenAIAPIKey)
	}
	v, err := s.View(ctx)
	if err != nil {
		t.Fatalf("View with one unreadable key: %v", err)
	}
	want := map[string]string{"openrouter_api_key": KeyNone, "anthropic_api_key": KeyStored, "openai_api_key": KeyEnvironment}
	for name, state := range want {
		if v.Keys[name] != state {
			t.Errorf("key %s is %q, want %q", name, v.Keys[name], state)
		}
	}

	if err := s.Apply(ctx, Patch{Keys: map[string]string{"openrouter_api_key": "sk-entered-again"}}); err != nil {
		t.Fatalf("entering the key again: %v", err)
	}
	if cfg, err = s.Effective(ctx); err != nil || cfg.OpenRouterAPIKey != "sk-entered-again" {
		t.Fatalf("after entering it again: %q, %v", cfg.OpenRouterAPIKey, err)
	}
	if v, _ = s.View(ctx); v.Keys["openrouter_api_key"] != KeyStored {
		t.Errorf("key shows %q after being entered again", v.Keys["openrouter_api_key"])
	}
}
