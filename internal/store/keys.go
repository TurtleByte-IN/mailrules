package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// Tenant data keys (MAI-150; see internal/crypto/keys.go). The store seals and opens every
// secret it keeps with the key of the secret's tenant (tenants.dek_enc), which it unwraps
// through the store's KeyProvider (WithKeys, WithKeyring).

// ErrNoKeys means the store was given no KeyProvider, so it can seal or open no secret.
var ErrNoKeys = errors.New("store: no key provider")

// DefaultTenantKeyCache is how long an unwrapped tenant key stays in memory unless the
// Keyring is told otherwise (MAILRULES_TENANT_KEY_CACHE).
const DefaultTenantKeyCache = time.Hour

// Keyring is a KeyProvider and the tenant keys it has unwrapped. Each stays in memory for
// a bounded time, then is unwrapped again, so a tenant whose key the provider no longer
// unwraps (a key service that went away) can open nothing once that time is up. Keys are
// held by tenant, KEK and wrapped key: one that changes (a new tenant under a reused id, a
// rotation) is unwrapped anew. Requests that need the same key at once share one unwrap.
// Every Store that seals with the same provider should share one Keyring (the app's and the
// worker's connections), so a tenant's key is unwrapped once for both.
type Keyring struct {
	p      crypto.KeyProvider  // the provider, its failures other than a wrong key made ErrKeyUnavailable
	legacy crypto.LegacyOpener // what opens envelope 1; nil when nothing does
	ttl    time.Duration
	now    func() time.Time
	mu     sync.Mutex
	keys   map[string]*unwrapped
}

// unwrapped is one tenant key, being unwrapped until done is closed.
type unwrapped struct {
	done       chan struct{}
	key        []byte
	err        error
	until      time.Time
	callerGone bool // the request that unwrapped it ended first: its error is not the provider's
}

// NewKeyring is a Keyring for p that keeps a key unwrapped for ttl (DefaultTenantKeyCache
// when ttl is not positive).
func NewKeyring(p crypto.KeyProvider, ttl time.Duration) *Keyring {
	if ttl <= 0 {
		ttl = DefaultTenantKeyCache
	}
	legacy, _ := p.(crypto.LegacyOpener)
	return &Keyring{p: available{p}, legacy: legacy, ttl: ttl, now: time.Now, keys: map[string]*unwrapped{}}
}

// WithKeys is the store sealing and opening secrets with p's key-encryption key, on a
// Keyring of its own that keeps keys DefaultTenantKeyCache. It shares the database of s.
func (s *Store) WithKeys(p crypto.KeyProvider) *Store {
	return s.WithKeyring(NewKeyring(p, DefaultTenantKeyCache))
}

// WithKeyring is the store sealing and opening secrets with k. It shares the database of s.
func (s *Store) WithKeyring(k *Keyring) *Store {
	return &Store{db: s.db, keys: k}
}

// legacy is what opens the secrets sealed before tenant keys, nil when nothing does.
func (s *Store) legacy() crypto.LegacyOpener {
	if s.keys == nil || s.keys.legacy == nil {
		return nil
	}
	return s.keys.legacy
}

// available is a provider whose failures, other than a wrong key, are ErrKeyUnavailable:
// a key service that cannot answer must never read as a key that is gone, which would
// let ReplaceUnopenableTenantKeys drop it.
type available struct{ crypto.KeyProvider }

func (a available) Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error) {
	w, err := a.KeyProvider.Wrap(ctx, tenantID, key)
	return w, unavailable(err)
}

func (a available) Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error) {
	k, err := a.KeyProvider.Unwrap(ctx, tenantID, keyID, wrapped)
	return k, unavailable(err)
}

func unavailable(err error) error {
	if err == nil || errors.Is(err, crypto.ErrDecrypt) || errors.Is(err, crypto.ErrKeyUnavailable) {
		return err
	}
	return fmt.Errorf("%w: %w", crypto.ErrKeyUnavailable, err)
}

// unwrap is the tenant's key, from memory while it is there and younger than the
// Keyring's ttl, else from the provider: once, however many requests ask at the same time.
// A failure is not kept: the next request asks the provider again.
func (k *Keyring) unwrap(ctx context.Context, tenantID int64, kekID string, wrapped []byte) ([]byte, error) {
	id := strconv.FormatInt(tenantID, 10) + "\x00" + kekID + "\x00" + string(wrapped)
	for {
		k.mu.Lock()
		u := k.keys[id]
		if u != nil {
			select {
			case <-u.done:
				if u.err == nil && k.now().Before(u.until) {
					k.mu.Unlock()
					return u.key, nil
				}
				u = nil // its time is up: unwrapped again
			default: // being unwrapped: wait for it below
			}
		}
		if u == nil {
			u = &unwrapped{done: make(chan struct{})}
			k.keys[id] = u
			k.mu.Unlock()
			return k.fill(ctx, id, u, tenantID, kekID, wrapped)
		}
		k.mu.Unlock()
		select {
		case <-u.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if u.err == nil {
			return u.key, nil
		}
		if !u.callerGone || ctx.Err() != nil {
			return nil, u.err
		}
		// The request that asked ended before the provider answered; this one asks again.
	}
}

// fill asks the provider for u, which unwrap has put under id, and lets go of it once its
// time is up.
func (k *Keyring) fill(ctx context.Context, id string, u *unwrapped, tenantID int64, kekID string, wrapped []byte) ([]byte, error) {
	key, err := k.p.Unwrap(ctx, tenantID, kekID, wrapped)
	k.mu.Lock()
	defer k.mu.Unlock()
	u.key, u.err, u.until, u.callerGone = key, err, k.now().Add(k.ttl), ctx.Err() != nil
	if err != nil {
		k.forgetLocked(id, u)
	} else {
		time.AfterFunc(k.ttl, func() {
			k.mu.Lock()
			defer k.mu.Unlock()
			k.forgetLocked(id, u)
		})
	}
	close(u.done)
	return key, err
}

// forgetLocked drops u from memory, unless another unwrap of the same key replaced it.
func (k *Keyring) forgetLocked(id string, u *unwrapped) {
	if k.keys[id] == u {
		delete(k.keys, id)
	}
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
	return s.keys.unwrap(ctx, tenantID, kekID, wrapped)
}

// ReplaceUnopenableTenantKeys drops every tenant key the store's key provider cannot unwrap,
// so the next secret sealed for that tenant gets a new key: what MAILRULES_NEW_MASTER_KEY
// asks for once the old master key is gone for good. The secrets sealed under a dropped key
// can never be opened again, so they go with it, in the same transaction, as ForgetIdentity
// deletes secrets: a mailbox shows its password deleted (Account.SecretGone), a destination
// its address deleted, a model key is not set. The person enters them again. It returns how
// many tenants' keys it dropped; with the right key it drops none. Only a wrong key
// (crypto.ErrDecrypt) drops one: a provider that cannot answer stops it with
// crypto.ErrKeyUnavailable, having dropped nothing more.
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
	if _, err := s.unwrap(ctx, tenantID, kekID, wrapped); !errors.Is(err, crypto.ErrDecrypt) {
		return false, err // it opens (nil), or the provider could not say: nothing is dropped
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

// RewrapTenantKeys wraps again, under the store's key provider, every tenant key wrapped
// under another KEK (tenants.kek_id): what moving an install from the master key to a
// module's key service (ext.Module.Keys) does, once per tenant. The secrets stay as they
// are, since they are sealed under the tenant's key, which does not change. Each tenant is
// one compare-and-set, so a second run, or two daemons at once, rewrap nothing twice.
// A tenant key that does not unwrap (crypto.ErrDecrypt: the master key is not the one it
// was wrapped under, or is gone) is left as it is; MasterKeyUses still counts it. A provider
// that cannot answer stops it with crypto.ErrKeyUnavailable: what is left is rewrapped at
// the next run. It returns how many it rewrapped.
func (s *Store) RewrapTenantKeys(ctx context.Context) (int, error) {
	if s.keys == nil {
		return 0, ErrNoKeys
	}
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("rewrap tenant keys: %w", err)
	}
	n := 0
	for _, t := range tenants {
		done, err := s.rewrapTenantKey(WithTenant(ctx, t), t)
		if err != nil {
			return n, fmt.Errorf("rewrap the key of tenant %d: %w", t, err)
		}
		if done {
			n++
		}
	}
	return n, nil
}

func (s *Store) rewrapTenantKey(ctx context.Context, tenantID int64) (bool, error) {
	to := s.keys.p.KeyID()
	wrapped, from, err := readTenantKey(ctx, s.db, tenantID)
	if err != nil || wrapped == nil || from == to {
		return false, err
	}
	key, err := s.unwrap(ctx, tenantID, from, wrapped)
	if errors.Is(err, crypto.ErrDecrypt) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rewrapped, err := s.keys.p.Wrap(ctx, tenantID, key)
	if err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tenants SET dek_enc = ?, kek_id = ? WHERE id = ? AND kek_id = ? AND dek_enc = ?`,
		rewrapped, to, tenantID, from, wrapped)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	return changed == 1, err
}

// MasterKeyUses counts what only the master key opens: tenant keys wrapped under it
// (kek_id "master"), secrets sealed under it before tenant keys (envelope 1), and removed
// mailboxes' copies sealed under it (envelope 1, or envelope 3 under "master"). In a build
// whose module holds the keys, the master key can be retired once all are 0.
type MasterKeyUses struct {
	TenantKeys, Secrets, RemovedMailboxes int
}

// Total is everything that still needs the master key.
func (u MasterKeyUses) Total() int { return u.TenantKeys + u.Secrets + u.RemovedMailboxes }

// MasterKeyUses counts, over every tenant, what still needs the master key. It needs no
// key itself.
func (s *Store) MasterKeyUses(ctx context.Context) (MasterKeyUses, error) {
	var u MasterKeyUses
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return u, fmt.Errorf("count master key uses: %w", err)
	}
	for _, t := range tenants {
		tctx := WithTenant(ctx, t)
		wrapped, kekID, err := readTenantKey(tctx, s.db, t)
		if err != nil {
			return u, fmt.Errorf("count master key uses: %w", err)
		}
		if wrapped != nil && kekID == crypto.MasterKeyID {
			u.TenantKeys++
		}
		secrets, err := s.tenantSecrets(tctx, s.db, t)
		if err != nil {
			return u, fmt.Errorf("count master key uses: %w", err)
		}
		for _, v := range secrets {
			if crypto.Envelope(v.dekEnc) == 1 {
				u.Secrets++
			}
		}
	}
	err = s.eachTenant(ctx, tenantsRemoved, func(ctx context.Context, _ int64) error {
		rs, err := s.db.QueryContext(ctx, `SELECT dek_enc FROM removed_mailboxes`)
		if err != nil {
			return err
		}
		for rs.Next() {
			var dekEnc []byte
			if err := rs.Scan(&dekEnc); err != nil {
				_ = rs.Close()
				return err
			}
			if crypto.Envelope(dekEnc) == 1 || crypto.KEKID(dekEnc) == crypto.MasterKeyID {
				u.RemovedMailboxes++
			}
		}
		return errors.Join(rs.Err(), rs.Close())
	})
	if err != nil {
		return u, fmt.Errorf("count master key uses: %w", err)
	}
	return u, nil
}

// Kinds of secret, bound into each ciphertext with its row id or name.
const (
	secretAccount     = "account"
	secretDestination = "destination"
	secretSetting     = "setting"
	secretRemoved     = "removed"
)
