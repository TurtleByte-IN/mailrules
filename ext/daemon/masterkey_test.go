package daemon

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/store/storetest"
)

func testKey(b byte) crypto.MasterKey { return crypto.MasterKey(bytes.Repeat([]byte{b}, 32)) }

func keyB64(b byte) string { return base64.StdEncoding.EncodeToString(testKey(b)) }

// installDir makes a data directory the way an install leaves it: master.key holding
// key(1) (when withKey), and one mailbox whose password is sealed under it. The store it
// returns has no keys, as openMasterKey takes it.
func installDir(t *testing.T, withKey bool) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	_, db := storetest.InDir(t, dir)
	st := store.New(db)
	u, err := st.CreateFirstUser(t.Context(), "me@example.test", "hash", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WithKeys(testKey(1)).CreateAccount(t.Context(), store.Account{UserID: u.ID, Label: "a", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "u", CreatedAt: 1}, "app password"); err != nil {
		t.Fatal(err)
	}
	if withKey {
		if err := os.WriteFile(crypto.MasterKeyFile(dir), []byte(keyB64(1)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, st
}

// emptyStore is a data directory with an empty database, and its store without keys.
func emptyStore(t *testing.T) (string, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	_, db := storetest.InDir(t, dir)
	return dir, store.New(db)
}

func loadCfg(t *testing.T, dir string, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.Load(nil, func(k string) string {
		if k == "MAILRULES_DATA_DIR" {
			return dir
		}
		return env[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestOpenMasterKey(t *testing.T) {
	t.Run("a fresh install gets a generated key", func(t *testing.T) {
		dir, st := emptyStore(t)
		key, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st)
		if err != nil || len(key) != 32 {
			t.Fatalf("key = %d bytes, %v", len(key), err)
		}
		got, err := crypto.LoadMasterKey("", "", dir)
		if err != nil || !bytes.Equal(got, key) {
			t.Fatalf("master.key does not hold the key that was returned: %v", err)
		}
		// A second start reads it back.
		again, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st)
		if err != nil || !bytes.Equal(again, key) {
			t.Fatalf("second start: %v", err)
		}
	})

	t.Run("a data directory that does not exist yet", func(t *testing.T) {
		_, st := emptyStore(t)
		dir := filepath.Join(t.TempDir(), "not", "yet")
		if _, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); err != nil {
			t.Fatal(err)
		}
		if !fileExists(crypto.MasterKeyFile(dir)) {
			t.Error("no master.key generated")
		}
	})

	t.Run("the install's own key is used", func(t *testing.T) {
		dir, st := installDir(t, true)
		key, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st)
		if err != nil || !bytes.Equal(key, testKey(1)) {
			t.Fatalf("key = %x, %v", key, err)
		}
	})

	t.Run("the key from the environment or a file, when it is the right one", func(t *testing.T) {
		dir, st := installDir(t, false)
		if key, err := openMasterKey(t.Context(), loadCfg(t, dir, map[string]string{"MAILRULES_MASTER_KEY": keyB64(1)}), st); err != nil || !bytes.Equal(key, testKey(1)) {
			t.Fatalf("env: %v", err)
		}
		f := filepath.Join(t.TempDir(), "k")
		if err := os.WriteFile(f, []byte(keyB64(1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if key, err := openMasterKey(t.Context(), loadCfg(t, dir, map[string]string{"MAILRULES_MASTER_KEY_FILE": f}), st); err != nil || !bytes.Equal(key, testKey(1)) {
			t.Fatalf("file: %v", err)
		}
		if fileExists(crypto.MasterKeyFile(dir)) {
			t.Error("a master.key was generated although the key came from elsewhere")
		}
	})
}

// The lost key: secrets in the database, and no key to open them with.
func TestOpenMasterKeyRefusesToReplaceALostKey(t *testing.T) {
	dir, st := installDir(t, false) // a restore that forgot master.key
	_, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st)
	if !errors.Is(err, ErrMasterKeyLost) {
		t.Fatalf("err = %v, want ErrMasterKeyLost", err)
	}
	if fileExists(crypto.MasterKeyFile(dir)) {
		t.Fatal("a new master.key was written although it would orphan the stored secrets")
	}
	abs, _ := filepath.Abs(dir)
	for _, want := range []string{abs, "master.key", "1 stored mailbox password", "MAILRULES_NEW_MASTER_KEY=true", "--new-master-key", "MAILRULES_MASTER_KEY_FILE", "docs/guide/backup.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
	// Asking again does not change that.
	if _, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); !errors.Is(err, ErrMasterKeyLost) || fileExists(crypto.MasterKeyFile(dir)) {
		t.Fatalf("second try: %v", err)
	}
}

func TestOpenMasterKeyStartFresh(t *testing.T) {
	dir, st := installDir(t, false)
	cfg := loadCfg(t, dir, map[string]string{"MAILRULES_NEW_MASTER_KEY": "true"})
	key, err := openMasterKey(t.Context(), cfg, st)
	if err != nil || len(key) != 32 || bytes.Equal(key, testKey(1)) {
		t.Fatalf("opt-in: key %d bytes, %v", len(key), err)
	}
	onDisk, err := crypto.LoadMasterKey("", "", dir)
	if err != nil || !bytes.Equal(onDisk, key) {
		t.Fatalf("the new key was not stored: %v", err)
	}
	// The old password, which nothing can open any more, is gone: the mailbox asks for it.
	if _, err := st.WithKeys(key).AccountSecret(t.Context(), 1); !errors.Is(err, store.ErrNoSecret) {
		t.Fatalf("old secret under the new key: %v", err)
	}
	// With nothing stored that the new key cannot open, a start without the opt-in goes on.
	if again, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); err != nil || !bytes.Equal(again, key) {
		t.Fatalf("restart before entering the password again: %v", err)
	}
	if err := st.WithKeys(key).SetAccountSecret(t.Context(), 1, "entered again"); err != nil {
		t.Fatal(err)
	}
	if again, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); err != nil || !bytes.Equal(again, key) {
		t.Fatalf("restart after entering it again: %v", err)
	}
}

// After MAILRULES_NEW_MASTER_KEY, with a key made anew or one given that is not the old one,
// the secrets sealed under the old key are gone (nothing could open them again), and
// entering them again works: a mailbox password and a model key are stored under the new
// key and read back, and the next start needs no opt-in.
func TestNewMasterKeyLetsSecretsBeEnteredAgain(t *testing.T) {
	for _, tt := range []struct {
		name string
		env  map[string]string // with MAILRULES_NEW_MASTER_KEY
	}{
		{"a new key is made", nil},
		{"another key is given", map[string]string{"MAILRULES_MASTER_KEY": keyB64(7)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			dir, st := installDir(t, false) // the mailbox password is under key(1), which is gone
			old := &settings.Settings{Store: st.WithKeys(testKey(1)), Env: loadCfg(t, dir, nil)}
			if err := old.Apply(ctx, store.SelfHostTenant, settings.Patch{Keys: map[string]string{"openrouter_api_key": "sk-or-old"}}); err != nil {
				t.Fatal(err)
			}

			cfg := loadCfg(t, dir, mergeEnv(tt.env, map[string]string{"MAILRULES_NEW_MASTER_KEY": "true"}))
			key, err := openMasterKey(ctx, cfg, st)
			if err != nil || bytes.Equal(key, testKey(1)) {
				t.Fatalf("opt-in: %v", err)
			}
			keyed := st.WithKeys(key)
			if _, err := keyed.AccountSecret(ctx, 1); !errors.Is(err, store.ErrNoSecret) {
				t.Fatalf("old password under the new key: %v", err)
			}
			sett := &settings.Settings{Store: keyed, Env: cfg}
			v, err := sett.View(ctx, store.SelfHostTenant)
			if err != nil || v.Keys["openrouter_api_key"] != settings.KeyNone {
				t.Fatalf("old model key after the new key: %q, %v", v.Keys["openrouter_api_key"], err)
			}
			if check, err := keyed.CheckSecrets(ctx); err != nil || check.Total != 0 {
				t.Fatalf("secrets left after the new key: %+v, %v", check, err)
			}

			if err := keyed.SetAccountSecret(ctx, 1, "entered again"); err != nil {
				t.Fatalf("entering the password again: %v", err)
			}
			if got, err := keyed.AccountSecret(ctx, 1); err != nil || got != "entered again" {
				t.Fatalf("password read back = %q, %v", got, err)
			}
			if err := sett.Apply(ctx, store.SelfHostTenant, settings.Patch{Keys: map[string]string{"openrouter_api_key": "sk-or-new"}}); err != nil {
				t.Fatalf("entering the model key again: %v", err)
			}
			if eff, err := sett.Effective(ctx, store.SelfHostTenant); err != nil || eff.OpenRouterAPIKey != "sk-or-new" {
				t.Fatalf("model key read back = %q, %v", eff.OpenRouterAPIKey, err)
			}

			// The next start, without the opt-in, opens them.
			again, err := openMasterKey(ctx, loadCfg(t, dir, tt.env), st)
			if err != nil || !bytes.Equal(again, key) {
				t.Fatalf("restart without the opt-in: %v", err)
			}
		})
	}
}

// A key that is there but is not the one the secrets were sealed under.
func TestOpenMasterKeyWrongKey(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte(keyB64(7)), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		env    map[string]string
		source string
	}{
		{"environment", map[string]string{"MAILRULES_MASTER_KEY": keyB64(7)}, "MAILRULES_MASTER_KEY"},
		{"key file", map[string]string{"MAILRULES_MASTER_KEY_FILE": keyFile}, keyFile},
		{"master.key", nil, "master.key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, st := installDir(t, false)
			if tt.env == nil {
				if err := os.WriteFile(crypto.MasterKeyFile(dir), []byte(keyB64(7)+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := openMasterKey(t.Context(), loadCfg(t, dir, tt.env), st)
			if !errors.Is(err, ErrMasterKeyLost) {
				t.Fatalf("err = %v, want ErrMasterKeyLost", err)
			}
			for _, want := range []string{tt.source, "opens none of the 1 stored", "MAILRULES_NEW_MASTER_KEY=true"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("message lacks %q:\n%s", want, err)
				}
			}
			if strings.Contains(err.Error(), keyB64(7)) || strings.Contains(err.Error(), keyB64(1)) {
				t.Error("the message shows a key")
			}

			// With the opt-in it goes on with the key it was given, and makes no other.
			cfg := loadCfg(t, dir, mergeEnv(tt.env, map[string]string{"MAILRULES_NEW_MASTER_KEY": "true"}))
			key, err := openMasterKey(t.Context(), cfg, st)
			if err != nil || !bytes.Equal(key, testKey(7)) {
				t.Fatalf("opt-in: %x, %v", key, err)
			}
		})
	}
}

func mergeEnv(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range []map[string]string{a, b} {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// Some secrets sealed under another key is a warning: those rows say decrypt failed one by
// one, and the others work. Here a second tenant's key is wrapped under another master key.
func TestOpenMasterKeySomeUnreadable(t *testing.T) {
	dir, st := installDir(t, true)
	u, err := st.SignInIdentity(t.Context(), store.Identity{Provider: "p", Subject: "s", Email: "b@example.test", Tenant: "other"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.WithKeys(testKey(5)).CreateAccount(t.Context(), store.Account{UserID: u.ID, Label: "b", Preset: "generic", Host: "h", Port: 993, TLSMode: "implicit", Username: "u2", CreatedAt: 1}, "other"); err != nil {
		t.Fatal(err)
	}
	if key, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); err != nil || !bytes.Equal(key, testKey(1)) {
		t.Fatalf("half the secrets readable: %v", err)
	}
	if got, err := st.WithKeys(testKey(5)).AccountSecret(t.Context(), 2); err != nil || got != "other" {
		t.Fatalf("without the opt-in the other tenant's key is kept: %q, %v", got, err)
	}
}

func TestOpenMasterKeyOnlyAProviderKeyStored(t *testing.T) {
	dir, st := emptyStore(t)
	// A provider key entered in the browser while the install had another key.
	sett := &settings.Settings{Store: st.WithKeys(testKey(1)), Env: loadCfg(t, dir, nil)}
	if err := sett.Apply(t.Context(), store.SelfHostTenant, settings.Patch{Keys: map[string]string{"openrouter_api_key": "sk-or-secret"}}); err != nil {
		t.Fatal(err)
	}
	// There is no master.key, so a new one would orphan it.
	if _, err := openMasterKey(t.Context(), loadCfg(t, dir, nil), st); !errors.Is(err, ErrMasterKeyLost) {
		t.Fatalf("a stored provider key must also count: %v", err)
	}
	if fileExists(crypto.MasterKeyFile(dir)) {
		t.Fatal("a master.key was written")
	}
	// With the right key present, the same database starts.
	env := map[string]string{"MAILRULES_MASTER_KEY": keyB64(1)}
	if key, err := openMasterKey(t.Context(), loadCfg(t, dir, env), st); err != nil || !bytes.Equal(key, testKey(1)) {
		t.Fatalf("with the right key: %v", err)
	}
}

func TestOpenMasterKeyNamedFileMissing(t *testing.T) {
	dir, st := installDir(t, true)
	cfg := loadCfg(t, dir, map[string]string{"MAILRULES_MASTER_KEY_FILE": filepath.Join(t.TempDir(), "gone.key"), "MAILRULES_NEW_MASTER_KEY": "true"})
	_, err := openMasterKey(t.Context(), cfg, st)
	if err == nil || errors.Is(err, ErrMasterKeyLost) {
		t.Fatalf("a named key file that is missing stays an error, with or without the opt-in: %v", err)
	}
}

// serve stops before it listens, and says why.
func TestServeStopsOnALostKey(t *testing.T) {
	dir, _ := installDir(t, false)
	cfg := loadCfg(t, dir, map[string]string{"MAILRULES_LISTEN": "127.0.0.1:0", "LOG_LEVEL": "error"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := serve(ctx, cfg, "test", nil)
	if !errors.Is(err, ErrMasterKeyLost) {
		t.Fatalf("serve = %v, want ErrMasterKeyLost", err)
	}
	if fileExists(crypto.MasterKeyFile(dir)) {
		t.Fatal("serve wrote a master.key")
	}
}

// The accounts commands read the key too, and must not make a new one either.
func TestAccountsListRefusesToReplaceALostKey(t *testing.T) {
	dir, _ := installDir(t, false)
	env := map[string]string{"MAILRULES_DATA_DIR": dir}
	var out syncBuffer
	err := accountsCLI{stdin: strings.NewReader(""), stdout: &out, getenv: func(k string) string { return env[k] }}.run(t.Context(), []string{"list"})
	if !errors.Is(err, ErrMasterKeyLost) {
		t.Fatalf("accounts list = %v, want ErrMasterKeyLost", err)
	}
	if fileExists(crypto.MasterKeyFile(dir)) {
		t.Fatal("accounts list wrote a master.key")
	}
}
