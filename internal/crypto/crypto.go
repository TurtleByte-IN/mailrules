// Package crypto holds envelope encryption for mailbox credentials and password hashing.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/argon2"
)

const keyLen = 32

// ErrDecrypt means a ciphertext was tampered with, moved between rows, or sealed under another key.
var ErrDecrypt = errors.New("decrypt failed")

// LoadMasterKey returns the key-encryption key: from the base64 value, else from keyFile,
// else from <dataDir>/master.key, which is generated (mode 0600) on first run.
func LoadMasterKey(value, keyFile, dataDir string) ([]byte, error) {
	if value == "" {
		generated := keyFile == ""
		if generated {
			keyFile = filepath.Join(dataDir, "master.key")
		}
		b, err := os.ReadFile(keyFile) // #nosec G304 -- operator-chosen path
		switch {
		case err == nil:
			value = strings.TrimSpace(string(b))
		case generated && errors.Is(err, os.ErrNotExist):
			key := make([]byte, keyLen)
			if _, err := rand.Read(key); err != nil {
				return nil, fmt.Errorf("generate master key: %w", err)
			}
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return nil, fmt.Errorf("create data dir: %w", err)
			}
			enc := base64.StdEncoding.EncodeToString(key) + "\n"
			if err := os.WriteFile(keyFile, []byte(enc), 0o600); err != nil {
				return nil, fmt.Errorf("write master key: %w", err)
			}
			return key, nil
		default:
			return nil, fmt.Errorf("read master key file: %w", err)
		}
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != keyLen {
		return nil, errors.New("master key must be 32 bytes, base64-encoded")
	}
	return key, nil
}

// Seal encrypts plaintext under a fresh data key and wraps that key with the master key.
// rowID is bound into both ciphertexts, so they cannot be swapped between rows.
func Seal(master []byte, rowID int64, plaintext []byte) (secretEnc, dekEnc []byte, err error) {
	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, nil, fmt.Errorf("generate data key: %w", err)
	}
	if secretEnc, err = seal(dek, rowID, plaintext); err != nil {
		return nil, nil, err
	}
	if dekEnc, err = seal(master, rowID, dek); err != nil {
		return nil, nil, err
	}
	return secretEnc, dekEnc, nil
}

// Open reverses Seal. It returns ErrDecrypt for any mismatch of key, row or ciphertext.
func Open(master []byte, rowID int64, secretEnc, dekEnc []byte) ([]byte, error) {
	dek, err := open(master, rowID, dekEnc)
	if err != nil {
		return nil, err
	}
	return open(dek, rowID, secretEnc)
}

func gcm(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("init cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

func aad(rowID int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(rowID)) // #nosec G115 -- bit pattern only
}

// seal returns nonce || ciphertext.
func seal(key []byte, rowID int64, plaintext []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, a.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return a.Seal(nonce, nonce, plaintext, aad(rowID)), nil
}

func open(key []byte, rowID int64, sealed []byte) ([]byte, error) {
	a, err := gcm(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < a.NonceSize() {
		return nil, ErrDecrypt
	}
	out, err := a.Open(nil, sealed[:a.NonceSize()], sealed[a.NonceSize():], aad(rowID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// argon2id parameters from the backend plan: 64 MB, 3 iterations, 2 lanes.
const (
	argonMemory  = 64 * 1024
	argonTime    = 3
	argonThreads = 2
	argonSaltLen = 16
)

// HashPassword returns an argon2id hash in the standard encoded form.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	sum := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, keyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

// VerifyPassword reports whether password matches an encoded hash, in constant time.
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want))) // #nosec G115 -- hash length
	return subtle.ConstantTimeCompare(got, want) == 1
}
