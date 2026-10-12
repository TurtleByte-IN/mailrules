package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// Tenant data keys (MAI-150; see internal/crypto/keys.go). The store seals and opens every
// secret it keeps with the key of the secret's tenant (tenants.dek_enc), which it unwraps
// through the store's KeyProvider (WithKeys).

// ErrNoKeys means the store was given no KeyProvider, so it can seal or open no secret.
var ErrNoKeys = errors.New("store: no key provider")

// keyring is the store's KeyProvider, and the tenant keys it has unwrapped, by wrapped key:
// one that changes (a new tenant under a reused id, a rotation) is unwrapped anew.
type keyring struct {
	p    crypto.KeyProvider
	mu   sync.Mutex
	keys map[string][]byte
}

// WithKeys is the store sealing and opening secrets with p's key-encryption key. It shares
// the database of s.
func (s *Store) WithKeys(p crypto.KeyProvider) *Store {
	return &Store{db: s.db, keys: &keyring{p: p, keys: map[string][]byte{}}}
}

// legacy is what opens the secrets sealed before tenant keys, nil when nothing does.
func (s *Store) legacy() crypto.LegacyOpener {
	if s.keys == nil {
		return nil
	}
	l, _ := s.keys.p.(crypto.LegacyOpener)
	return l
}

// TenantKey is the tenant's data key, made (and stored wrapped) when it has none yet.
func (s *Store) TenantKey(ctx context.Context, tenantID int64) (crypto.TenantKey, error) {
	return s.tenantKey(WithTenant(ctx, tenantID), s.db, tenantID)
}

// tenantKey is TenantKey read, and made, in q: a transaction that is already writing must
// pass itself, since another connection could not write meanwhile.
func (s *Store) tenantKey(ctx context.Context, q querier, tenantID int64) (crypto.TenantKey, error) {
	if s.keys == nil {
		return crypto.TenantKey{}, ErrNoKeys
	}
	wrapped, kekID, err := readTenantKey(ctx, q, tenantID)
	if err != nil {
		return crypto.TenantKey{}, err
	}
	if wrapped == nil {
		if wrapped, kekID, err = s.makeTenantKey(ctx, q, tenantID); err != nil {
			return crypto.TenantKey{}, err
		}
	}
	key, err := s.unwrap(ctx, tenantID, kekID, wrapped)
	if err != nil {
		return crypto.TenantKey{}, fmt.Errorf("open the key of tenant %d: %w", tenantID, err)
	}
	return crypto.NewTenantKey(tenantID, key, s.legacy()), nil
}

// readTenantKey is the tenant's wrapped data key and the KEK it is wrapped under; nil when
// it has none, ErrNotFound for no such tenant.
func readTenantKey(ctx context.Context, q querier, tenantID int64) ([]byte, string, error) {
	var wrapped []byte
	var kekID string
	err := q.QueryRowContext(ctx, `SELECT dek_enc, kek_id FROM tenants WHERE id = ?`, tenantID).Scan(&wrapped, &kekID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("tenant %d: %w", tenantID, ErrNotFound)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the key of tenant %d: %w", tenantID, err)
	}
	return wrapped, kekID, nil
}

// makeTenantKey stores a new data key for a tenant that has none. Of two made at once one
// is stored, and both callers get it.
func (s *Store) makeTenantKey(ctx context.Context, q querier, tenantID int64) ([]byte, string, error) {
	key, err := crypto.NewDataKey()
	if err != nil {
		return nil, "", err
	}
	wrapped, err := s.keys.p.Wrap(ctx, tenantID, key)
	if err != nil {
		return nil, "", fmt.Errorf("wrap the key of tenant %d: %w", tenantID, err)
	}
	if _, err := q.ExecContext(ctx, `UPDATE tenants SET dek_enc = ?, kek_id = ? WHERE id = ? AND dek_enc IS NULL`,
		wrapped, s.keys.p.KeyID(), tenantID); err != nil {
		return nil, "", fmt.Errorf("store the key of tenant %d: %w", tenantID, err)
	}
	stored, kekID, err := readTenantKey(ctx, q, tenantID)
	if err == nil && stored == nil {
		err = fmt.Errorf("store the key of tenant %d: %w", tenantID, ErrNotFound)
	}
	return stored, kekID, err
}

func (s *Store) unwrap(ctx context.Context, tenantID int64, kekID string, wrapped []byte) ([]byte, error) {
	k := s.keys
	id := strconv.FormatInt(tenantID, 10) + "\x00" + kekID + "\x00" + string(wrapped)
	k.mu.Lock()
	key, ok := k.keys[id]
	k.mu.Unlock()
	if ok {
		return key, nil
	}
	key, err := k.p.Unwrap(ctx, tenantID, kekID, wrapped)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	k.keys[id] = key
	k.mu.Unlock()
	return key, nil
}

// ReplaceUnopenableTenantKeys drops every tenant key the store's key provider cannot unwrap,
// so the next secret sealed for that tenant gets a new key: what MAILRULES_NEW_MASTER_KEY
// asks for once the old master key is gone for good. The secrets sealed under a dropped key
// can never be opened again, so they go with it, in the same transaction, as ForgetIdentity
// deletes secrets: a mailbox shows its password deleted (Account.SecretGone), a destination
// its address deleted, a model key is not set. The person enters them again. It returns how
// many tenants' keys it dropped; with the right key it drops none.
func (s *Store) ReplaceUnopenableTenantKeys(ctx context.Context) (int, error) {
	if s.keys == nil {
		return 0, ErrNoKeys
	}
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("replace tenant keys: %w", err)
	}
	n := 0
	for _, t := range tenants {
		dropped, err := s.replaceTenantKey(WithTenant(ctx, t), t)
		if err != nil {
			return n, fmt.Errorf("replace the key of tenant %d: %w", t, err)
		}
		if dropped {
			n++
		}
	}
	return n, nil
}

func (s *Store) replaceTenantKey(ctx context.Context, tenantID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	wrapped, kekID, err := readTenantKey(ctx, tx, tenantID)
	if err != nil || wrapped == nil {
		return false, err
	}
	if _, err := s.unwrap(ctx, tenantID, kekID, wrapped); err == nil {
		return false, nil
	}
	secrets, err := s.tenantSecrets(ctx, tx, tenantID)
	if err != nil {
		return false, err
	}
	for _, v := range secrets {
		if crypto.Envelope(v.dekEnc) != 2 {
			continue // not under the tenant's key
		}
		if v.table == "settings" {
			_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE tenant_id = ? AND key = ?`, tenantID, keyPrefix+v.name)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE `+v.table+` SET secret_enc = ?, dek_enc = ? WHERE id = ?`, []byte{}, []byte{}, v.id)
		}
		if err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE tenants SET dek_enc = NULL, kek_id = '' WHERE id = ?`, tenantID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Kinds of secret, bound into each ciphertext with its row id or name.
const (
	secretAccount     = "account"
	secretDestination = "destination"
	secretSetting     = "setting"
	secretRemoved     = "removed"
)
