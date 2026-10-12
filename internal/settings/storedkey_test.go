package settings

import (
	"bytes"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

// A provider key sealed under a key that is gone does not take the other settings down: it
// reads as not set, the screen says so, and entering the key again replaces it. A damaged
// row is not a key the tenant's key opens either, but it is no wrong key: it is an error.
func TestUnreadableStoredKeyIsNotSet(t *testing.T) {
	ctx := t.Context()
	st, _ := storetest.New(t)
	if _, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	own, err := st.TenantKey(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	gone := crypto.NewTenantKey(1, bytes.Repeat([]byte{9}, 32), nil) // a data key this install no longer has
	for name, k := range map[string]crypto.TenantKey{"openrouter_api_key": gone, "anthropic_api_key": own} {
		enc, err := store.SealSetting(k, name, "sk-secret")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetSetting(ctx, 1, keyPrefix+name, enc); err != nil {
			t.Fatal(err)
		}
	}
	env, err := config.Load(nil, func(k string) string { return map[string]string{"OPENAI_API_KEY": "sk-from-env"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{Store: st, Env: env}

	cfg, err := s.Effective(ctx, 1)
	if err != nil {
		t.Fatalf("Effective with one unreadable key: %v", err)
	}
	if cfg.OpenRouterAPIKey != "" || cfg.AnthropicAPIKey != "sk-secret" || cfg.OpenAIAPIKey != "sk-from-env" {
		t.Errorf("keys in force = %q %q %q", cfg.OpenRouterAPIKey, cfg.AnthropicAPIKey, cfg.OpenAIAPIKey)
	}
	v, err := s.View(ctx, 1)
	if err != nil {
		t.Fatalf("View with one unreadable key: %v", err)
	}
	want := map[string]string{"openrouter_api_key": KeyNone, "anthropic_api_key": KeyStored, "openai_api_key": KeyEnvironment}
	for name, state := range want {
		if v.Keys[name] != state {
			t.Errorf("key %s is %q, want %q", name, v.Keys[name], state)
		}
	}

	if err := s.Apply(ctx, 1, Patch{Keys: map[string]string{"openrouter_api_key": "sk-entered-again"}}); err != nil {
		t.Fatalf("entering the key again: %v", err)
	}
	if cfg, err = s.Effective(ctx, 1); err != nil || cfg.OpenRouterAPIKey != "sk-entered-again" {
		t.Fatalf("after entering it again: %q, %v", cfg.OpenRouterAPIKey, err)
	}
	if v, _ = s.View(ctx, 1); v.Keys["openrouter_api_key"] != KeyStored {
		t.Errorf("key shows %q after being entered again", v.Keys["openrouter_api_key"])
	}

	if err := st.SetSetting(ctx, 1, keyPrefix+"openai_api_key", "not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Effective(ctx, 1); err == nil {
		t.Error("Effective with a damaged stored key: no error")
	}
}

// A provider key is sealed under its tenant's key: another tenant's key does not open it.
func TestStoredKeyIsTheTenants(t *testing.T) {
	ctx := t.Context()
	st, _ := storetest.New(t)
	if _, err := st.CreateFirstUser(ctx, "me@example.test", "hash", 1); err != nil {
		t.Fatal(err)
	}
	env, err := config.Load(nil, func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	s := &Settings{Store: st, Env: env}
	if err := s.Apply(ctx, 1, Patch{Keys: map[string]string{"openrouter_api_key": "sk-mine"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.Settings(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	stored := rows[keyPrefix+"openrouter_api_key"]
	if bytes.Contains([]byte(stored), []byte("sk-mine")) {
		t.Fatal("the key is stored in the clear")
	}
	own, err := st.TenantKey(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.OpenSetting(own, "openrouter_api_key", stored); err != nil || got != "sk-mine" {
		t.Fatalf("OpenSetting with the tenant's key = %q, %v", got, err)
	}
	other := crypto.NewTenantKey(2, bytes.Repeat([]byte{1}, 32), nil)
	if _, err := store.OpenSetting(other, "openrouter_api_key", stored); err == nil {
		t.Error("another tenant's key opened the stored key")
	}
}
