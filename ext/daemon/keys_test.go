package daemon

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// kms is a module's key service: a key of its own, under its own id, that can stop
// answering.
type kms struct {
	down    atomic.Bool
	unwraps atomic.Int32
}

const kmsKey = "projects/p/locations/global/keyRings/mailrules/cryptoKeys/tenant-keys"

func (k *kms) KeyID() string { return kmsKey }

func (k *kms) Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error) {
	if k.down.Load() {
		return nil, errors.New("kms: unavailable")
	}
	return testKey(42).Wrap(ctx, tenantID, key)
}

func (k *kms) Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error) {
	k.unwraps.Add(1)
	if k.down.Load() {
		return nil, errors.New("kms: unavailable")
	}
	if keyID != kmsKey {
		return nil, ext.ErrWrongKey
	}
	return testKey(42).Unwrap(ctx, tenantID, crypto.MasterKeyID, wrapped)
}

// start is what serve does with the keys: open them, then bring the database under them.
func start(t *testing.T, dir string, st *store.Store, k *kms, env map[string]string) (*store.Store, keys, error) {
	t.Helper()
	cfg := loadCfg(t, dir, env)
	opened, err := openKeys(t.Context(), cfg, st, []ext.Module{{Name: "free"}, {Name: "kms", Keys: k}})
	if err != nil {
		return nil, keys{}, err
	}
	keyed := st.WithKeyring(opened.ring)
	return keyed, opened, upgradeKeys(t.Context(), cfg, keyed, opened)
}

func kekOf(t *testing.T, st *store.Store) string {
	t.Helper()
	uses, err := st.MasterKeyUses(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if uses.TenantKeys > 0 {
		return crypto.MasterKeyID
	}
	return kmsKey
}

// An install that used the master key moves to the module's key service at its first
// start with the module: once that start is done, the master key can go.
func TestStartWithAKeyService(t *testing.T) {
	dir, st := installDir(t, true)
	k := &kms{}

	// The first start finds the key service down: the tenant key stays under the master key,
	// which still opens it, and the next start moves it.
	k.down.Store(true)
	keyed, opened, err := start(t, dir, st, k, nil)
	if err != nil || !opened.module || !opened.master {
		t.Fatalf("start with the key service down: %+v, %v", opened, err)
	}
	if kekOf(t, st) != crypto.MasterKeyID {
		t.Fatal("the tenant key moved while the key service was down")
	}
	if pw, err := keyed.AccountSecret(t.Context(), 1); err != nil || pw != "app password" {
		t.Fatalf("with the key service down, the master key's: %q, %v", pw, err)
	}

	k.down.Store(false)
	if _, _, err := start(t, dir, st, k, nil); err != nil {
		t.Fatal(err)
	}
	if kekOf(t, st) != kmsKey {
		t.Fatal("the tenant key was not moved under the key service")
	}

	// Without the master key the next start goes on, and the password opens.
	if err := os.Remove(crypto.MasterKeyFile(dir)); err != nil {
		t.Fatal(err)
	}
	keyed, opened, err = start(t, dir, st, k, nil)
	if err != nil || opened.master {
		t.Fatalf("start without the master key: %+v, %v", opened, err)
	}
	if pw, err := keyed.AccountSecret(t.Context(), 1); err != nil || pw != "app password" {
		t.Fatalf("after the move, without the master key: %q, %v", pw, err)
	}
	if fileExists(crypto.MasterKeyFile(dir)) {
		t.Error("a master key was generated in a build whose module holds the keys")
	}
}

// Without the master key, while the database still holds what only it opens, the daemon
// refuses to start, unless told to go on without it.
func TestStartWithAKeyServiceNeedsTheMasterKeyUntilMoved(t *testing.T) {
	dir, st := installDir(t, false) // the password is under key(1), which is not set
	k := &kms{}
	if _, _, err := start(t, dir, st, k, nil); !errors.Is(err, ErrMasterKeyStillNeeded) {
		t.Fatalf("start: %v, want ErrMasterKeyStillNeeded", err)
	}
	if k.unwraps.Load() != 0 {
		t.Error("the key service was asked to unwrap what the master key wrapped")
	}
	keyed, _, err := start(t, dir, st, k, map[string]string{"MAILRULES_NEW_MASTER_KEY": "true"})
	if err != nil {
		t.Fatalf("start told to go on: %v", err)
	}
	if _, err := keyed.AccountSecret(t.Context(), 1); !errors.Is(err, store.ErrNoSecret) {
		t.Fatalf("the password only the master key opened: %v, want ErrNoSecret", err)
	}
	if err := keyed.SetAccountSecret(t.Context(), 1, "entered again"); err != nil {
		t.Fatal(err)
	}
	if kekOf(t, st) != kmsKey {
		t.Fatal("the new tenant key is not the key service's")
	}
	if _, _, err := start(t, dir, st, k, nil); err != nil {
		t.Fatalf("the next start: %v", err)
	}
}

func TestOpenKeysRefusesTwoModules(t *testing.T) {
	dir, st := emptyStore(t)
	_, err := openKeys(t.Context(), loadCfg(t, dir, nil), st, []ext.Module{{Name: "a", Keys: &kms{}}, {Name: "b", Keys: &kms{}}})
	if err == nil {
		t.Fatal("two modules that hold the keys were accepted")
	}
}
