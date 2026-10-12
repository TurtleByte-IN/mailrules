package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/api"
	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// keys is the Keyring every store of the process seals and opens secrets with, and where
// its key-encryption key comes from.
type keys struct {
	ring   *store.Keyring
	module bool // a module holds the keys (ext.Module.Keys)
	master bool // with module: the master key is set too, and unwraps what it wrapped
}

// openKeys opens the process's keys. Without a module that holds them they are the master
// key's (openMasterKey). With one they are the module's provider, with the master key, when
// one is set or in the data directory, still unwrapping what was wrapped under it; no
// master key is ever generated then. st is the store without keys.
func openKeys(ctx context.Context, cfg *config.Config, st *store.Store, modules []ext.Module) (keys, error) {
	p, err := api.KeyModule(modules)
	if err != nil {
		return keys{}, err
	}
	if p == nil {
		key, err := openMasterKey(ctx, cfg, st)
		if err != nil {
			return keys{}, err
		}
		return keys{ring: store.NewKeyring(key, cfg.TenantKeyCache)}, nil
	}
	var master crypto.MasterKey
	switch loaded, err := crypto.LoadMasterKey(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir); {
	case err == nil:
		master = loaded
	case !errors.Is(err, crypto.ErrNoMasterKey):
		return keys{}, err
	}
	return keys{ring: store.NewKeyring(crypto.WithMasterKey(p, master), cfg.TenantKeyCache), module: true, master: master != nil}, nil
}

// ErrMasterKeyStillNeeded means a build whose module holds the keys started without the
// master key while the database still holds keys or secrets only the master key opens.
var ErrMasterKeyStillNeeded = errors.New("master key still needed")

// upgradeKeys brings what the database holds under the store's key provider, once, at
// start: with a module that holds the keys, each tenant key the master key wrapped is
// rewrapped under the module's provider (store.RewrapTenantKeys); then every secret still
// sealed under the master key itself is resealed (store.UpgradeSecrets). Both are done at
// start rather than when a tenant's key is next used, so that one start says whether the
// master key can be retired, instead of waiting on tenants that may never sign in again.
// A key service that cannot be reached leaves the rest for the next start: what the master
// key wrapped still opens with it meanwhile. With a module, it then logs whether the
// master key is still needed, and refuses to start without it while something needs it,
// unless cfg.NewMasterKey says to go on with that unreadable.
func upgradeKeys(ctx context.Context, cfg *config.Config, st *store.Store, k keys) error {
	module := k.module
	if module {
		if !k.master {
			uses, err := st.MasterKeyUses(ctx)
			if err != nil {
				return err
			}
			if uses.Total() > 0 {
				if !cfg.NewMasterKey {
					return fmt.Errorf(`%w: this build keeps the keys in the key service of a module, but the database still holds %s only the master key opens, and no master key is set.
  1. Set MAILRULES_MASTER_KEY (or MAILRULES_MASTER_KEY_FILE) to the key this database used, then start again: MailRules moves them under the key service and says when the master key can go.
  2. If that key is gone for good, start once with MAILRULES_NEW_MASTER_KEY=true: what only it opened stays unreadable, and the mailbox passwords and model keys are entered again.
See docs/guide/security.md`, ErrMasterKeyStillNeeded, describeUses(uses))
				}
				if err := replaceTenantKeys(ctx, st); err != nil {
					return err
				}
				slog.Warn("started without the master key because MAILRULES_NEW_MASTER_KEY is set; what only it opened cannot be read until it is entered again",
					"tenant_keys", uses.TenantKeys, "secrets", uses.Secrets, "removed_mailboxes", uses.RemovedMailboxes)
			}
			return nil
		}
		n, err := st.RewrapTenantKeys(ctx)
		if n > 0 {
			slog.Info("rewrapped tenant keys under the key service", "tenants", n)
		}
		if err != nil && !errors.Is(err, crypto.ErrKeyUnavailable) {
			return err
		}
		if err != nil {
			slog.Warn("the key service cannot be reached; the remaining tenant keys stay under the master key until the next start", "error", err.Error())
		}
	}
	upgraded, err := st.UpgradeSecrets(ctx)
	if upgraded > 0 {
		slog.Info("resealed stored secrets under their tenant's key", "secrets", upgraded)
	}
	switch {
	case module && errors.Is(err, crypto.ErrKeyUnavailable):
		slog.Warn("the key service cannot be reached; the remaining secrets stay under the master key until the next start", "error", err.Error())
	case err != nil:
		return err
	}
	if !module {
		return nil
	}
	uses, err := st.MasterKeyUses(ctx)
	if err != nil {
		return err
	}
	if uses.Total() == 0 {
		slog.Info("nothing in the database needs the master key any more: it can be retired (unset MAILRULES_MASTER_KEY or MAILRULES_MASTER_KEY_FILE, and remove master.key from the data directory)")
		return nil
	}
	slog.Warn("the master key is still needed: keep MAILRULES_MASTER_KEY set until a start says it can be retired",
		"tenant_keys", uses.TenantKeys, "secrets", uses.Secrets, "removed_mailboxes", uses.RemovedMailboxes)
	return nil
}

// describeUses is what still needs the master key, for a message.
func describeUses(u store.MasterKeyUses) string {
	return fmt.Sprintf("%d tenant keys, %d secrets sealed before tenant keys and %d removed mailboxes' copies", u.TenantKeys, u.Secrets, u.RemovedMailboxes)
}
