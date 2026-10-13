package telemetry

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// address finds what looks like an email address: a local part of letters, digits and
// ._%+- , then @, then a domain of at least two labels whose last starts with a letter
// (so example.com, not v0.1.0). It is a net, not a parser: it errs towards redacting.
var address = regexp.MustCompile(`[\p{L}\p{N}._%+-]+@(?:[\p{L}\p{N}](?:[\p{L}\p{N}-]*[\p{L}\p{N}])?\.)+\p{L}[\p{L}\p{N}-]*`)

// AddressTag is what stands for addr in a log that redacts addresses: the first 8 hex
// digits of its HMAC-SHA256 under key, lowercased first, so the same address gets the
// same tag whatever its case, and no tag can be turned back into its address without key.
func AddressTag(key []byte, addr string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(strings.ToLower(addr)))
	return "<email:" + hex.EncodeToString(m.Sum(nil)[:4]) + ">"
}

// addressWriter replaces every address in what is written with its AddressTag. Each
// Write is one JSON line from slog: an escape sequence (\n, \u0001) never ends inside
// an address, but one can end just before it, and its letter is not part of the address.
type addressWriter struct {
	w   io.Writer
	key []byte
}

func (a addressWriter) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, '@') < 0 {
		return a.w.Write(p)
	}
	out := make([]byte, 0, len(p))
	last := 0
	for _, m := range address.FindAllIndex(p, -1) {
		start := skipEscape(p, m[0])
		if start >= m[1] || p[start] == '@' {
			continue // only an escape's letters stood before the @
		}
		out = append(out, p[last:start]...)
		out = append(out, AddressTag(a.key, string(p[start:m[1]]))...)
		last = m[1]
	}
	out = append(out, p[last:]...)
	if _, err := a.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// skipEscape returns where an address found at i in a JSON line starts: past the
// letters of a JSON escape (\n, \t, \u00e9) that ends at i, if one does.
func skipEscape(p []byte, i int) int {
	slashes := 0
	for j := i - 1; j >= 0 && p[j] == '\\'; j-- {
		slashes++
	}
	if slashes%2 == 0 {
		return i // no escape, or an escaped backslash
	}
	if p[i] == 'u' {
		return min(i+5, len(p))
	}
	return i + 1
}

// LogKeyFile is where the key of AddressTag is kept in dataDir.
func LogKeyFile(dataDir string) string { return filepath.Join(dataDir, "log.key") }

// ReadLogKey reads the key of AddressTag from dataDir; fs.ErrNotExist when there is none.
func ReadLogKey(dataDir string) ([]byte, error) {
	b, err := os.ReadFile(LogKeyFile(dataDir)) // #nosec G304 -- under the data directory
	if err != nil {
		return nil, fmt.Errorf("read log key: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 16 {
		return nil, fmt.Errorf("read log key: %s is not a base64 key of at least 16 bytes", LogKeyFile(dataDir))
	}
	return key, nil
}

// LogKey returns the key of AddressTag kept in dataDir, generating it the first time.
// It is kept apart from the master key so that tags stay the same when that key is
// replaced or retired (the hosted build keeps keys in a key service); it opens nothing,
// so losing it only changes every address's tag.
func LogKey(dataDir string) ([]byte, error) {
	key, err := ReadLogKey(dataDir)
	if !errors.Is(err, fs.ErrNotExist) {
		return key, err
	}
	key = make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate log key: %w", err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	f, err := os.OpenFile(LogKeyFile(dataDir), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- under the data directory
	if errors.Is(err, fs.ErrExist) {
		return ReadLogKey(dataDir) // another process made it first
	}
	if err != nil {
		return nil, fmt.Errorf("write log key: %w", err)
	}
	if _, err := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write log key: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write log key: %w", err)
	}
	return key, nil
}
