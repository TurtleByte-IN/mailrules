package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/crypto"
)

// A provider key set through the API is a settings row under keyPrefix whose value is the
// sealed key as JSON: both parts base64-encoded, sealed under the tenant's key bound to the
// setting's name (SealSetting).

// keyPrefix is the settings keys of the stored model API keys (settings.keyPrefix).
const keyPrefix = "key."

// sealedSetting is a sealed setting as stored.
type sealedSetting struct {
	Sealed string `json:"sealed"`
	DEK    string `json:"dek"`
}

// settingAAD is the number envelope 1 bound a sealed setting to: a hash of its name.
func settingAAD(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64()) // #nosec G115 -- bit pattern only
}

// SealSetting seals value for the settings row of name under the tenant's key k, as the
// row's JSON value.
func SealSetting(k crypto.TenantKey, name, value string) (string, error) {
	secret, dek, err := k.Seal(secretSetting, name, []byte(value))
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sealedSetting{base64.StdEncoding.EncodeToString(secret), base64.StdEncoding.EncodeToString(dek)})
	return string(b), err
}

// OpenSetting opens a value SealSetting stored for name with the tenant's key k, or one
// sealed before tenant keys. A wrong key is crypto.ErrDecrypt.
func OpenSetting(k crypto.TenantKey, name, stored string) (string, error) {
	secret, dek, err := decodeSetting(name, stored)
	if err != nil {
		return "", err
	}
	plain, err := k.Open(secretSetting, name, settingAAD(name), secret, dek)
	if err != nil {
		return "", fmt.Errorf("stored key %s: %w", name, err)
	}
	return string(plain), nil
}

func decodeSetting(name, stored string) (secret, dek []byte, err error) {
	var v sealedSetting
	if err := json.Unmarshal([]byte(stored), &v); err != nil {
		return nil, nil, fmt.Errorf("decode stored key %s: %w", name, err)
	}
	secret, err1 := base64.StdEncoding.DecodeString(v.Sealed)
	dek, err2 := base64.StdEncoding.DecodeString(v.DEK)
	if err := errors.Join(err1, err2); err != nil {
		return nil, nil, fmt.Errorf("decode stored key %s: %w", name, err)
	}
	return secret, dek, nil
}

// SecretsCheck counts the secrets the database holds sealed: mailbox passwords and tokens,
// provider keys and the addresses of notification destinations.
type SecretsCheck struct {
	Total      int // every stored secret
	Unreadable int // the ones the store's key provider cannot open
}

// Readable is how many secrets the key opens.
func (c SecretsCheck) Readable() int { return c.Total - c.Unreadable }

// CheckSecrets counts every tenant's stored secrets, and those the store's key provider
// cannot open: whether the key-encryption key is the one they are under, checked by
// unwrapping each tenant's key, not by opening each secret. A secret sealed before tenant
// keys (envelope 1) is opened, as it is the master key's own. A store without keys opens
// none, which is how the caller counts what a key would have to protect before it has one.
// A secret that fails is counted, not an error; only a database failure is.
func (s *Store) CheckSecrets(ctx context.Context) (SecretsCheck, error) {
	var c SecretsCheck
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return c, fmt.Errorf("check stored secrets: %w", err)
	}
	for _, t := range tenants {
		tc, err := s.checkTenant(WithTenant(ctx, t), t)
		if err != nil {
			return c, fmt.Errorf("check stored secrets: %w", err)
		}
		c.Total += tc.Total
		c.Unreadable += tc.Unreadable
	}
	return c, nil
}

// storedSecret is one sealed secret as CheckSecrets and UpgradeSecrets read it.
type storedSecret struct {
	table             string // accounts | notify_destinations | settings
	id                int64  // the row, for accounts and destinations
	name              string // the setting, for settings
	secretEnc, dekEnc []byte
}

// binding is what the secret is sealed to within its tenant.
func (v storedSecret) binding() (kind, ref string, legacyRow int64) {
	switch v.table {
	case "accounts":
		return secretAccount, strconv.FormatInt(v.id, 10), v.id
	case "notify_destinations":
		return secretDestination, strconv.FormatInt(v.id, 10), destinationAAD(v.id)
	}
	return secretSetting, v.name, settingAAD(v.name)
}

// tenantSecrets lists a tenant's stored secrets; a setting that does not decode is listed
// with no ciphertext, so it counts and never opens.
func (s *Store) tenantSecrets(ctx context.Context, q querier, tenantID int64) ([]storedSecret, error) {
	var out []storedSecret
	for _, table := range []string{"accounts", "notify_destinations"} {
		rs, err := q.QueryContext(ctx, `SELECT id, secret_enc, dek_enc FROM `+table+` WHERE tenant_id = ? AND length(secret_enc) > 0 ORDER BY id`, tenantID)
		if err != nil {
			return nil, err
		}
		for rs.Next() {
			v := storedSecret{table: table}
			if err := rs.Scan(&v.id, &v.secretEnc, &v.dekEnc); err != nil {
				_ = rs.Close()
				return nil, err
			}
			out = append(out, v)
		}
		if err := errors.Join(rs.Err(), rs.Close()); err != nil {
			return nil, err
		}
	}
	rs, err := q.QueryContext(ctx, `SELECT key, value FROM settings WHERE tenant_id = ? AND substr(key, 1, 4) = 'key.' ORDER BY key`, tenantID)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var key, value string
		if err := rs.Scan(&key, &value); err != nil {
			_ = rs.Close()
			return nil, err
		}
		v := storedSecret{table: "settings", name: strings.TrimPrefix(key, keyPrefix)}
		v.secretEnc, v.dekEnc, _ = decodeSetting(v.name, value)
		out = append(out, v)
	}
	return out, errors.Join(rs.Err(), rs.Close())
}

func (s *Store) checkTenant(ctx context.Context, tenantID int64) (SecretsCheck, error) {
	var c SecretsCheck
	secrets, err := s.tenantSecrets(ctx, s.db, tenantID)
	if err != nil {
		return c, err
	}
	c.Total = len(secrets)
	if s.keys == nil {
		c.Unreadable = c.Total
		return c, nil
	}
	wrapped, kekID, err := readTenantKey(ctx, s.db, tenantID)
	if err != nil {
		return c, err
	}
	keyOpens := false
	if wrapped != nil {
		_, uerr := s.unwrap(ctx, tenantID, kekID, wrapped)
		keyOpens = uerr == nil
	}
	legacy := s.legacy()
	for _, v := range secrets {
		switch crypto.Envelope(v.dekEnc) {
		case 1:
			_, _, row := v.binding()
			if legacy == nil {
				c.Unreadable++
			} else if _, err := legacy.OpenLegacy(row, v.secretEnc, v.dekEnc); err != nil {
				c.Unreadable++
			}
		case 2:
			if !keyOpens {
				c.Unreadable++
			}
		default:
			c.Unreadable++
		}
	}
	return c, nil
}

// UpgradeSecrets reseals every secret still sealed under the master key itself (envelope
// 1, before tenant keys) under its tenant's key, and every removed mailbox's copy under the
// key-encryption key (envelope 3), tenant by tenant, each in one transaction. It returns how
// many it resealed; a second run reseals none. The daemon runs it at start, after the key
// check; until it has, envelope 1 still opens wherever a secret is read. A secret the
// master key does not open is left as it is (CheckSecrets counts it).
func (s *Store) UpgradeSecrets(ctx context.Context) (int, error) {
	if s.keys == nil {
		return 0, ErrNoKeys
	}
	legacy := s.legacy()
	if legacy == nil {
		return 0, nil // nothing could have been sealed under this provider before
	}
	tenants, err := s.Tenants(ctx)
	if err != nil {
		return 0, fmt.Errorf("upgrade stored secrets: %w", err)
	}
	n := 0
	for _, t := range tenants {
		m, err := s.upgradeTenant(WithTenant(ctx, t), t, legacy)
		if err != nil {
			return n, fmt.Errorf("upgrade the secrets of tenant %d: %w", t, err)
		}
		n += m
	}
	m, err := s.upgradeRemoved(ctx, legacy)
	if err != nil {
		return n, fmt.Errorf("upgrade removed mailboxes: %w", err)
	}
	return n + m, nil
}

func (s *Store) upgradeTenant(ctx context.Context, tenantID int64, legacy crypto.LegacyOpener) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op after Commit
	secrets, err := s.tenantSecrets(ctx, tx, tenantID)
	if err != nil {
		return 0, err
	}
	var key crypto.TenantKey
	n := 0
	for _, v := range secrets {
		if crypto.Envelope(v.dekEnc) != 1 {
			continue
		}
		kind, ref, row := v.binding()
		plain, err := legacy.OpenLegacy(row, v.secretEnc, v.dekEnc)
		if err != nil {
			continue // not this key's: left for the person to enter again
		}
		if n == 0 {
			if key, err = s.tenantKey(ctx, tx, tenantID); err != nil {
				return 0, err
			}
		}
		var secretEnc, dekEnc []byte
		var value string
		if v.table == "settings" {
			value, err = SealSetting(key, v.name, string(plain))
		} else {
			secretEnc, dekEnc, err = key.Seal(kind, ref, plain)
		}
		if err != nil {
			return 0, err
		}
		if v.table == "settings" {
			_, err = tx.ExecContext(ctx, `UPDATE settings SET value = ? WHERE tenant_id = ? AND key = ?`, value, tenantID, keyPrefix+v.name)
		} else {
			_, err = tx.ExecContext(ctx, `UPDATE `+v.table+` SET secret_enc = ?, dek_enc = ? WHERE id = ?`, secretEnc, dekEnc, v.id)
		}
		if err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// upgradeRemoved reseals the removed mailboxes' envelope-1 copies under the KEK.
func (s *Store) upgradeRemoved(ctx context.Context, legacy crypto.LegacyOpener) (int, error) {
	n := 0
	err := s.eachTenant(ctx, tenantsRemoved, func(ctx context.Context, _ int64) error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // a no-op after Commit
		type row struct {
			id, accountID     int64
			secretEnc, dekEnc []byte
		}
		rs, err := tx.QueryContext(ctx, `SELECT id, account_id, secret_enc, dek_enc FROM removed_mailboxes ORDER BY id`)
		if err != nil {
			return err
		}
		var old []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.id, &r.accountID, &r.secretEnc, &r.dekEnc); err != nil {
				_ = rs.Close()
				return err
			}
			if crypto.Envelope(r.dekEnc) == 1 {
				old = append(old, r)
			}
		}
		if err := errors.Join(rs.Err(), rs.Close()); err != nil {
			return err
		}
		for _, r := range old {
			plain, err := legacy.OpenLegacy(r.accountID, r.secretEnc, r.dekEnc)
			if err != nil {
				continue
			}
			secretEnc, dekEnc, err := crypto.SealKEK(ctx, s.keys.p, removedBinding(r.accountID), plain)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE removed_mailboxes SET secret_enc = ?, dek_enc = ? WHERE id = ?`, secretEnc, dekEnc, r.id); err != nil {
				return err
			}
			n++
		}
		return tx.Commit()
	})
	return n, err
}
