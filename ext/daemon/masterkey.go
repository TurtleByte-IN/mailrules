package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/crypto"
	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
)

// ErrMasterKeyLost means the database holds mailbox passwords or model keys that the master
// key in use cannot open: the key is missing, or it is not the one they were sealed under.
// Starting would leave every mailbox stuck reconnecting, so the daemon refuses unless
// MAILRULES_NEW_MASTER_KEY says to go on.
var ErrMasterKeyLost = errors.New("master key lost")

type masterKeyLostError struct{ msg string }

func (e *masterKeyLostError) Error() string        { return e.msg }
func (e *masterKeyLostError) Is(target error) bool { return target == ErrMasterKeyLost }

// stored is "3 stored passwords and keys", for a message.
func stored(n int) string {
	if n == 1 {
		return "1 stored mailbox password or model key"
	}
	return fmt.Sprintf("%d stored mailbox passwords and model keys", n)
}

// openMasterKey returns the master key, and protects against the one mistake that is easy
// to make and hard to see: a restore that forgot master.key, or an environment that holds a
// different key, would otherwise give a working start with every mailbox failing to
// decrypt. It refuses to
//
//   - generate a new master.key while the database holds stored secrets, and
//   - go on with a key that opens none of the stored secrets,
//
// unless cfg.NewMasterKey is set. A fresh install, with no stored secrets, is unaffected:
// its key is generated as before. When only some secrets fail to open, it logs and goes on,
// since those rows are individual: a mailbox whose password was stored by another key.
func openMasterKey(ctx context.Context, cfg *config.Config, st *store.Store) ([]byte, error) {
	dataDir := cfg.DataDir
	if abs, err := filepath.Abs(dataDir); err == nil {
		dataDir = abs
	}
	key, err := crypto.LoadMasterKey(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir)
	if errors.Is(err, crypto.ErrNoMasterKey) {
		check, err := settings.CheckSecrets(ctx, st, nil)
		if err != nil {
			return nil, err
		}
		if check.Total > 0 && !cfg.NewMasterKey {
			return nil, &masterKeyLostError{fmt.Sprintf(`the master key is missing: there is no master.key in %[1]s, and neither MAILRULES_MASTER_KEY nor MAILRULES_MASTER_KEY_FILE is set. The database there holds %[2]s sealed under it, and MailRules will not make a new key: every one of them would become unreadable and every mailbox would sit in Reconnecting.
  1. Put master.key from your backup into %[1]s (or set MAILRULES_MASTER_KEY or MAILRULES_MASTER_KEY_FILE to the key), then start again.
  2. If the key is gone for good, start once with MAILRULES_NEW_MASTER_KEY=true (or --new-master-key). MailRules then makes a new key, and you enter the mailbox passwords and model keys again.
See docs/guide/backup.md`, dataDir, stored(check.Total))}
		}
		key, err := crypto.GenerateMasterKey(cfg.DataDir)
		if err != nil {
			return nil, err
		}
		if check.Total > 0 {
			slog.Warn("made a new master key because MAILRULES_NEW_MASTER_KEY is set; the stored mailbox passwords and model keys cannot be read until you enter them again",
				"stored_secrets", check.Total, "data_dir", dataDir)
		}
		return key, nil
	}
	if err != nil {
		return nil, err
	}

	check, err := settings.CheckSecrets(ctx, st, key)
	if err != nil {
		return nil, err
	}
	switch {
	case check.Total > 0 && check.Readable() == 0 && !cfg.NewMasterKey:
		return nil, &masterKeyLostError{fmt.Sprintf(`the master key from %[1]s opens none of the %[2]s in the database in %[3]s: it is not the key they were sealed under, so every mailbox would sit in Reconnecting.
  1. Put the right key back (the master.key of the install this database came from), then start again.
  2. If that key is gone for good, start once with MAILRULES_NEW_MASTER_KEY=true (or --new-master-key) to go on with this key, and enter the mailbox passwords and model keys again.
See docs/guide/backup.md`, crypto.MasterKeySource(cfg.MasterKey, cfg.MasterKeyFile, cfg.DataDir), stored(check.Total), dataDir)}
	case check.Total > 0 && check.Readable() == 0:
		slog.Warn("going on because MAILRULES_NEW_MASTER_KEY is set; the stored mailbox passwords and model keys cannot be read until you enter them again",
			"stored_secrets", check.Total, "data_dir", dataDir)
	case check.Unreadable > 0:
		slog.Warn("some stored mailbox passwords or model keys cannot be opened with this master key; enter them again where a mailbox says decrypt failed",
			"unreadable", check.Unreadable, "stored_secrets", check.Total)
	}
	return key, nil
}
