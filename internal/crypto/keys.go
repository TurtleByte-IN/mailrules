package crypto

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
)

// Tenant data keys (MAI-150). Every tenant has its own random data key (TDK), stored only
// wrapped by a key-encryption key (KEK) that a KeyProvider holds: the master key today,
// a KMS later (MAI-186). A secret is sealed under a fresh per-secret key, which the TDK
// wraps, and both are bound to (tenant, kind, row or name), so a ciphertext neither opens
// in another tenant nor in another row of the same one. Deleting a tenant's wrapped TDK
// makes everything sealed under it unreadable, wherever a copy of it went (crypto-shred);
// rotating the KEK rewraps one key per tenant, never the secrets.
//
// How a sealed secret is read is told by its wrapped per-secret key (dekEnc):
//
//	envelope 1: 60 bytes, nonce || GCM(master, key, row id): before tenant keys
//	envelope 2: 0x02 || nonce || GCM(TDK, key, binding)
//	envelope 3: 0x03 || len(kek id) || kek id || KEK-wrapped key: a copy that must outlive
//	            its tenant's key (a removed mailbox waiting for its module)

// KeyProvider holds a key-encryption key: it wraps tenants' data keys and unwraps them.
// tenantID is bound into each wrap, so a wrapped key does not open as another tenant's.
type KeyProvider interface {
	// KeyID names the KEK new keys are wrapped under; it is stored with each wrapped key,
	// so a provider holding several (a rotation) knows which one opens it.
	KeyID() string
	Wrap(ctx context.Context, tenantID int64, key []byte) ([]byte, error)
	// Unwrap returns ErrDecrypt when keyID or the tenant is not the one wrapped.
	Unwrap(ctx context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error)
}

// LegacyOpener opens secrets sealed before tenant keys (envelope 1), with the master key
// they were sealed under.
type LegacyOpener interface {
	OpenLegacy(rowID int64, secretEnc, dekEnc []byte) ([]byte, error)
}

// MasterKey is the KeyProvider of the master key (LoadMasterKey), the KEK of self-host and
// of the hosted build until a KMS holds it. It also opens envelope 1.
type MasterKey []byte

var (
	_ KeyProvider  = MasterKey(nil)
	_ LegacyOpener = MasterKey(nil)
)

// MasterKeyID is the KeyID of MasterKey.
const MasterKeyID = "master"

// KeyID implements KeyProvider.
func (m MasterKey) KeyID() string { return MasterKeyID }

// Wrap implements KeyProvider.
func (m MasterKey) Wrap(_ context.Context, tenantID int64, key []byte) ([]byte, error) {
	return seal(m, wrapAAD(tenantID), key)
}

// Unwrap implements KeyProvider.
func (m MasterKey) Unwrap(_ context.Context, tenantID int64, keyID string, wrapped []byte) ([]byte, error) {
	if keyID != MasterKeyID {
		return nil, ErrDecrypt
	}
	return open(m, wrapAAD(tenantID), wrapped)
}

// OpenLegacy implements LegacyOpener.
func (m MasterKey) OpenLegacy(rowID int64, secretEnc, dekEnc []byte) ([]byte, error) {
	return openV1(m, rowID, secretEnc, dekEnc)
}

// wrapAAD binds a wrapped data key to its tenant.
func wrapAAD(tenantID int64) []byte {
	return binary.BigEndian.AppendUint64([]byte("mailrules/tenant-key\x00"), uint64(tenantID)) // #nosec G115 -- bit pattern only
}

// NewDataKey makes a random data key for a tenant.
func NewDataKey() ([]byte, error) {
	key := make([]byte, keyLen)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate tenant key: %w", err)
	}
	return key, nil
}

// Binding is what a secret is sealed to: its tenant, its kind ("account", "destination",
// "setting", "removed") and its row id or name. LegacyRow is the number envelope 1 bound
// it to, for opening one sealed before tenant keys.
type Binding struct {
	TenantID  int64
	Kind, Ref string
	LegacyRow int64
}

// aad is the binding as additional data: length-prefixed, so no two bindings share one.
func (b Binding) aad(envelope byte) []byte {
	out := append([]byte("mailrules/secret\x00"), envelope)
	out = binary.BigEndian.AppendUint64(out, uint64(b.TenantID)) // #nosec G115 -- bit pattern only
	for _, s := range []string{b.Kind, b.Ref} {
		out = binary.BigEndian.AppendUint32(out, uint32(len(s))) // #nosec G115 -- a name, far below 4 GB
		out = append(out, s...)
	}
	return out
}

// Envelopes, by the first byte of a wrapped per-secret key.
const (
	envelopeTenant byte = 2
	envelopeKEK    byte = 3
)

// legacyLen is the length of an envelope-1 wrapped key: nonce, 32-byte key, GCM tag.
const legacyLen = 12 + keyLen + 16

// Envelope reports how a secret was sealed, by its wrapped per-secret key: 1, 2 or 3, and 0
// for none (an empty, deleted secret) or one it does not know.
func Envelope(dekEnc []byte) int {
	switch {
	case len(dekEnc) == legacyLen:
		return 1
	case len(dekEnc) > 1 && (dekEnc[0] == envelopeTenant || dekEnc[0] == envelopeKEK):
		return int(dekEnc[0])
	}
	return 0
}

// TenantKey is one tenant's unwrapped data key, with what opens the tenant's secrets sealed
// before it existed. It never leaves the process.
type TenantKey struct {
	tenantID int64
	key      []byte
	legacy   LegacyOpener // nil: envelope 1 does not open
}

// NewTenantKey is tenant's data key, unwrapped; legacy may be nil.
func NewTenantKey(tenantID int64, key []byte, legacy LegacyOpener) TenantKey {
	return TenantKey{tenantID: tenantID, key: key, legacy: legacy}
}

// Tenant is the tenant the key is of.
func (k TenantKey) Tenant() int64 { return k.tenantID }

// Seal seals plaintext under a fresh per-secret key wrapped by the tenant's key (envelope
// 2), bound to kind and ref within the tenant.
func (k TenantKey) Seal(kind, ref string, plaintext []byte) (secretEnc, dekEnc []byte, err error) {
	b := Binding{TenantID: k.tenantID, Kind: kind, Ref: ref}
	dek, err := NewDataKey()
	if err != nil {
		return nil, nil, err
	}
	if secretEnc, err = seal(dek, b.aad(envelopeTenant), plaintext); err != nil {
		return nil, nil, err
	}
	wrapped, err := seal(k.key, b.aad(envelopeTenant), dek)
	if err != nil {
		return nil, nil, err
	}
	return secretEnc, append([]byte{envelopeTenant}, wrapped...), nil
}

// Open opens a secret of the tenant sealed by Seal under kind and ref, or one sealed before
// tenant keys (envelope 1) to legacyRow. It returns ErrDecrypt for a wrong key, tenant,
// kind, row or ciphertext.
func (k TenantKey) Open(kind, ref string, legacyRow int64, secretEnc, dekEnc []byte) ([]byte, error) {
	switch Envelope(dekEnc) {
	case 1:
		if k.legacy == nil {
			return nil, ErrDecrypt
		}
		return k.legacy.OpenLegacy(legacyRow, secretEnc, dekEnc)
	case int(envelopeTenant):
		b := Binding{TenantID: k.tenantID, Kind: kind, Ref: ref}
		dek, err := open(k.key, b.aad(envelopeTenant), dekEnc[1:])
		if err != nil {
			return nil, err
		}
		return open(dek, b.aad(envelopeTenant), secretEnc)
	}
	return nil, ErrDecrypt
}

// MAC is an HMAC of data under a key derived from the tenant's key for label: a fingerprint
// that ties a stored value to a secret without storing the secret.
func (k TenantKey) MAC(label string, data []byte) []byte {
	sub := hmac.New(sha256.New, k.key)
	_, _ = sub.Write([]byte("mailrules/mac\x00" + label))
	m := hmac.New(sha256.New, sub.Sum(nil))
	_, _ = m.Write(data)
	return m.Sum(nil)
}

// SealKEK seals plaintext under a fresh per-secret key that the provider wraps itself
// (envelope 3), bound to b: for a copy that must stay readable after its tenant's key is
// gone. The per-secret key is wrapped as tenant b.TenantID's.
func SealKEK(ctx context.Context, p KeyProvider, b Binding, plaintext []byte) (secretEnc, dekEnc []byte, err error) {
	dek, err := NewDataKey()
	if err != nil {
		return nil, nil, err
	}
	if secretEnc, err = seal(dek, b.aad(envelopeKEK), plaintext); err != nil {
		return nil, nil, err
	}
	wrapped, err := p.Wrap(ctx, b.TenantID, dek)
	if err != nil {
		return nil, nil, fmt.Errorf("wrap key: %w", err)
	}
	id := p.KeyID()
	if len(id) > 255 {
		return nil, nil, fmt.Errorf("key id %q is too long", id)
	}
	head := append([]byte{envelopeKEK, byte(len(id))}, id...) // #nosec G115 -- at most 255, checked above
	return secretEnc, append(head, wrapped...), nil
}

// OpenKEK reverses SealKEK; with legacy, it also opens envelope 1 bound to b.LegacyRow.
func OpenKEK(ctx context.Context, p KeyProvider, legacy LegacyOpener, b Binding, secretEnc, dekEnc []byte) ([]byte, error) {
	switch Envelope(dekEnc) {
	case 1:
		if legacy == nil {
			return nil, ErrDecrypt
		}
		return legacy.OpenLegacy(b.LegacyRow, secretEnc, dekEnc)
	case int(envelopeKEK):
		n := int(dekEnc[1])
		if len(dekEnc) < 2+n {
			return nil, ErrDecrypt
		}
		dek, err := p.Unwrap(ctx, b.TenantID, string(dekEnc[2:2+n]), dekEnc[2+n:])
		if err != nil {
			return nil, err
		}
		return open(dek, b.aad(envelopeKEK), secretEnc)
	}
	return nil, ErrDecrypt
}
