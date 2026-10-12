package crypto

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, keyLen) }

// Envelope 1, as versions before tenant keys sealed: still opened by the master key.
func TestEnvelopeOne(t *testing.T) {
	master := key(1)
	secret, dek, err := sealV1(master, 7, []byte("abcd-efgh-ijkl-mnop"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(secret, []byte("abcd")) {
		t.Fatal("ciphertext contains plaintext")
	}
	if Envelope(dek) != 1 {
		t.Fatalf("envelope %d, want 1", Envelope(dek))
	}
	got, err := MasterKey(master).OpenLegacy(7, secret, dek)
	if err != nil || string(got) != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	otherSecret, otherDek, _ := sealV1(master, 8, []byte("other"))
	flipped := append([]byte(nil), secret...)
	flipped[len(flipped)-1] ^= 1
	fails := []struct {
		name        string
		master      []byte
		row         int64
		secret, dek []byte
	}{
		{"wrong row id", master, 8, secret, dek},
		{"secret swapped from another row", master, 7, otherSecret, dek},
		{"data key swapped from another row", master, 7, secret, otherDek},
		{"whole row swapped", master, 7, otherSecret, otherDek},
		{"wrong master key", key(2), 7, secret, dek},
		{"tampered ciphertext", master, 7, flipped, dek},
		{"truncated", master, 7, secret[:4], dek},
	}
	for _, tt := range fails {
		if _, err := MasterKey(tt.master).OpenLegacy(tt.row, tt.secret, tt.dek); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: want ErrDecrypt, got %v", tt.name, err)
		}
	}
	// A tenant's key opens the tenant's envelope-1 secrets through the master key.
	tk := NewTenantKey(1, key(9), MasterKey(master))
	if got, err := tk.Open("account", "7", 7, secret, dek); err != nil || string(got) != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("tenant key on envelope 1: %q, %v", got, err)
	}
	if _, err := NewTenantKey(1, key(9), nil).Open("account", "7", 7, secret, dek); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("envelope 1 without the master key: %v", err)
	}
}

// A tenant's data key, wrapped by the master key, seals each secret to its tenant, kind and
// row: it opens nowhere else.
func TestTenantKey(t *testing.T) {
	ctx := t.Context()
	master := MasterKey(key(1))
	dk, err := NewDataKey()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := master.Wrap(ctx, 5, dk)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := master.Unwrap(ctx, 5, MasterKeyID, wrapped); err != nil || !bytes.Equal(got, dk) {
		t.Fatalf("unwrap: %v", err)
	}
	for name, unwrap := range map[string]func() ([]byte, error){
		"another tenant": func() ([]byte, error) { return master.Unwrap(ctx, 6, MasterKeyID, wrapped) },
		"another KEK id": func() ([]byte, error) { return master.Unwrap(ctx, 5, "kms:1", wrapped) },
		"another master": func() ([]byte, error) { return MasterKey(key(2)).Unwrap(ctx, 5, MasterKeyID, wrapped) },
	} {
		if _, err := unwrap(); !errors.Is(err, ErrDecrypt) {
			t.Errorf("unwrap as %s: %v", name, err)
		}
	}

	k := NewTenantKey(5, dk, master)
	secret, dek, err := k.Seal("account", "7", []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if Envelope(dek) != 2 {
		t.Fatalf("envelope %d, want 2", Envelope(dek))
	}
	if got, err := k.Open("account", "7", 7, secret, dek); err != nil || string(got) != "pw" {
		t.Fatalf("round trip: %q, %v", got, err)
	}
	other, _ := NewDataKey()
	for _, tt := range []struct {
		name      string
		k         TenantKey
		kind, ref string
	}{
		{"another tenant with the same key", NewTenantKey(6, dk, master), "account", "7"},
		{"another tenant's key", NewTenantKey(5, other, master), "account", "7"},
		{"another row", k, "account", "8"},
		{"another kind", k, "destination", "7"},
		// Length-prefixed: ("accoun", "t7") reads "account7" run together, as ("account", "7") does.
		{"a binding that runs together alike", k, "accoun", "t7"},
	} {
		if _, err := tt.k.Open(tt.kind, tt.ref, 7, secret, dek); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: want ErrDecrypt, got %v", tt.name, err)
		}
	}
	if _, err := k.Open("account", "7", 7, secret, []byte{2}); !errors.Is(err, ErrDecrypt) {
		t.Errorf("a truncated key: %v", err)
	}
	if a, b := k.MAC("x", []byte("d")), NewTenantKey(5, other, nil).MAC("x", []byte("d")); bytes.Equal(a, b) || !bytes.Equal(a, k.MAC("x", []byte("d"))) {
		t.Error("MAC is not a function of the tenant's key")
	}
}

// A copy sealed under the KEK itself opens with the provider alone, after any tenant key is gone.
func TestSealKEK(t *testing.T) {
	ctx := t.Context()
	master := MasterKey(key(1))
	b := Binding{Kind: "removed", Ref: "7", LegacyRow: 7}
	secret, dek, err := SealKEK(ctx, master, b, []byte("token"))
	if err != nil {
		t.Fatal(err)
	}
	if Envelope(dek) != 3 {
		t.Fatalf("envelope %d, want 3", Envelope(dek))
	}
	if got, err := OpenKEK(ctx, master, master, b, secret, dek); err != nil || string(got) != "token" {
		t.Fatalf("round trip: %q, %v", got, err)
	}
	if _, err := OpenKEK(ctx, master, master, Binding{Kind: "removed", Ref: "8"}, secret, dek); !errors.Is(err, ErrDecrypt) {
		t.Errorf("another row: %v", err)
	}
	if _, err := OpenKEK(ctx, MasterKey(key(2)), nil, b, secret, dek); !errors.Is(err, ErrDecrypt) {
		t.Errorf("another KEK: %v", err)
	}
	old, oldDek, _ := sealV1(key(1), 7, []byte("old"))
	if got, err := OpenKEK(ctx, master, master, b, old, oldDek); err != nil || string(got) != "old" {
		t.Errorf("envelope 1 copy: %q, %v", got, err)
	}
}

func TestLoadMasterKey(t *testing.T) {
	dir := t.TempDir()
	// Loading never makes a key: a missing master.key is its own error.
	if _, err := LoadMasterKey("", "", dir); !errors.Is(err, ErrNoMasterKey) {
		t.Fatalf("empty data dir: want ErrNoMasterKey, got %v", err)
	}
	if _, err := os.Stat(MasterKeyFile(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("LoadMasterKey created a key file: %v", err)
	}

	generated, err := GenerateMasterKey(dir)
	if err != nil || len(generated) != keyLen {
		t.Fatalf("generate: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "master.key"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("master.key mode: %v %v", info, err)
	}
	again, err := LoadMasterKey("", "", dir)
	if err != nil || !bytes.Equal(generated, again) {
		t.Fatal("load did not read the generated key")
	}
	// It never replaces a key that is there.
	if _, err := GenerateMasterKey(dir); err == nil {
		t.Fatal("GenerateMasterKey replaced an existing master.key")
	}
	if kept, _ := LoadMasterKey("", "", dir); !bytes.Equal(generated, kept) {
		t.Fatal("the existing master.key changed")
	}
	// It makes the data directory when it is not there yet.
	if _, err := GenerateMasterKey(filepath.Join(dir, "new", "data")); err != nil {
		t.Fatalf("generate into a missing directory: %v", err)
	}

	fromEnv, err := LoadMasterKey(base64.StdEncoding.EncodeToString(key(9)), "", dir)
	if err != nil || !bytes.Equal(fromEnv, key(9)) {
		t.Fatalf("env value: %v", err)
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := LoadMasterKey(bad, "", dir); err == nil {
			t.Errorf("accepted bad key %q", bad)
		}
	}
	// A named key file that is missing is an error of its own kind: nothing may be made there.
	_, err = LoadMasterKey("", filepath.Join(dir, "missing.key"), dir)
	if err == nil || errors.Is(err, ErrNoMasterKey) {
		t.Errorf("a named key file that is missing must be a plain error, got %v", err)
	}
}

func TestMasterKeySource(t *testing.T) {
	if got := MasterKeySource("abc", "", "/d"); got != "MAILRULES_MASTER_KEY" {
		t.Errorf("env: %q", got)
	}
	if got := MasterKeySource("", "/k/file", "/d"); !strings.Contains(got, "/k/file") || strings.Contains(got, "abc") {
		t.Errorf("file: %q", got)
	}
	if got := MasterKeySource("", "", "/d"); got != filepath.Join("/d", "master.key") {
		t.Errorf("default: %q", got)
	}
}

func TestPassword(t *testing.T) {
	hash, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !VerifyPassword(hash, "correct horse battery") {
		t.Error("right password rejected")
	}
	if VerifyPassword(hash, "correct horse batterz") {
		t.Error("wrong password accepted")
	}
	for _, bad := range []string{"", "plain", "$argon2i$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=1,t=1,p=1$!!$aGFzaA"} {
		if VerifyPassword(bad, "x") {
			t.Errorf("malformed hash %q accepted", bad)
		}
	}
}
