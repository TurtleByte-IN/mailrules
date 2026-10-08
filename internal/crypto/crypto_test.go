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

func TestSealOpen(t *testing.T) {
	master := key(1)
	secret, dek, err := Seal(master, 7, []byte("abcd-efgh-ijkl-mnop"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(secret, []byte("abcd")) {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := Open(master, 7, secret, dek)
	if err != nil || string(got) != "abcd-efgh-ijkl-mnop" {
		t.Fatalf("round trip: %q, %v", got, err)
	}

	otherSecret, otherDek, _ := Seal(master, 8, []byte("other"))
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
		if _, err := Open(tt.master, tt.row, tt.secret, tt.dek); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: want ErrDecrypt, got %v", tt.name, err)
		}
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
