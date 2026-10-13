package telemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

// With a key, no address reaches the log or the error writers, wherever in the record it
// was: the message, an attribute, a nested group, an error, a context's attributes.
func TestLoggerRedactsAddresses(t *testing.T) {
	var logged, errs bytes.Buffer
	log := NewLogger(&logged, "debug", testKey, &errs).With("account", "Me@iCloud.com")
	log.Error("cannot sign in as me@icloud.com",
		"user", "me@icloud.com",
		slog.Group("imap", "host", "imap.mail.me.com", slog.Group("from", "addr", "Ann <ann.lee+news@example.co.uk>")),
		"error", fmt.Errorf("login: %w", errors.New(`NO [AUTHENTICATIONFAILED] "bob@example.org" refused`)),
		"list", []string{"carol@example.net", "dave@xn--bcher-kva.example"},
		"note", "line one\nzoë@exämple.de")
	for name, out := range map[string]string{"log": logged.String(), "error writer": errs.String()} {
		for _, leak := range []string{"icloud.com", "iCloud.com", "ann.lee", "bob@", "carol@", "dave@", "zoë@", "@"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s holds %q: %s", name, leak, out)
			}
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(out), &line); err != nil {
			t.Fatalf("%s is no longer JSON: %v\n%s", name, err, out)
		}
		tag := AddressTag(testKey, "me@icloud.com")
		if line["msg"] != "cannot sign in as "+tag || line["user"] != tag || line["account"] != tag {
			t.Errorf("%s: the same address, whatever its case, is not one tag %s: %s", name, tag, out)
		}
		if imap := line["imap"].(map[string]any); imap["host"] != "imap.mail.me.com" ||
			imap["from"].(map[string]any)["addr"] != "Ann <"+AddressTag(testKey, "ann.lee+news@example.co.uk")+">" {
			t.Errorf("%s: group: %v", name, imap)
		}
		if line["note"] != "line one\n"+AddressTag(testKey, "zoë@exämple.de") {
			t.Errorf("%s: an address after an escape: %q", name, line["note"])
		}
	}
	if errs.String() != logged.String() {
		t.Errorf("error writer got\n%s\nthe log\n%s", errs.String(), logged.String())
	}
}

func TestAddressWriter(t *testing.T) {
	tag := func(a string) string { return AddressTag(testKey, a) }
	tests := []struct{ name, in, want string }{
		{"no @", `{"msg":"listening","listen":"127.0.0.1:8080"}`, `{"msg":"listening","listen":"127.0.0.1:8080"}`},
		{"bare address", `{"user":"me@example.com"}`, `{"user":"` + tag("me@example.com") + `"}`},
		{"two in one string", `{"msg":"a@x.io to b@y.io"}`, `{"msg":"` + tag("a@x.io") + ` to ` + tag("b@y.io") + `"}`},
		{"quoted in an error", `{"error":"user \"me@x.io\" refused"}`, `{"error":"user \"` + tag("me@x.io") + `\" refused"}`},
		{"after \\n", `{"msg":"a\nme@x.io"}`, `{"msg":"a\n` + tag("me@x.io") + `"}`},
		{"after \\u0001", `{"msg":"\u0001me@x.io"}`, `{"msg":"\u0001` + tag("me@x.io") + `"}`},
		{"after an escaped backslash", `{"msg":"\\me@x.io"}`, `{"msg":"\\` + tag("me@x.io") + `"}`},
		{"in a URL", `{"url":"imaps://me@x.io:993/INBOX"}`, `{"url":"imaps://` + tag("me@x.io") + `:993/INBOX"}`},
		{"a module version", `{"dep":"golang.org/x/net@v0.1.0"}`, `{"dep":"golang.org/x/net@v0.1.0"}`},
		{"a handle", `{"msg":"ask @tilak"}`, `{"msg":"ask @tilak"}`},
		{"a host with no dot", `{"user":"root@localhost"}`, `{"user":"root@localhost"}`},
		{"only an escape before @", `{"msg":"\n@x.io"}`, `{"msg":"\n@x.io"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			n, err := addressWriter{w: &out, key: testKey}.Write([]byte(tt.in))
			if err != nil || n != len(tt.in) {
				t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(tt.in))
			}
			if out.String() != tt.want {
				t.Errorf("got  %s\nwant %s", out.String(), tt.want)
			}
		})
	}
}

// A line without @ is written as it came, without allocating.
func TestAddressWriterNoAt(t *testing.T) {
	var out bytes.Buffer
	out.Grow(1024)
	w := addressWriter{w: &out, key: testKey}
	line := []byte(`{"time":"2026-10-13T10:00:00Z","level":"INFO","msg":"listening"}` + "\n")
	if allocs := testing.AllocsPerRun(100, func() { out.Reset(); _, _ = w.Write(line) }); allocs != 0 {
		t.Errorf("%v allocations for a line without @", allocs)
	}
	if !bytes.Equal(out.Bytes(), line) {
		t.Errorf("changed: %s", out.String())
	}
}

// A tag depends on the address and the key alone, never on the logger that wrote it.
func TestAddressTag(t *testing.T) {
	if !regexp.MustCompile(`^<email:[0-9a-f]{8}>$`).MatchString(AddressTag(testKey, "me@x.io")) {
		t.Errorf("tag %q", AddressTag(testKey, "me@x.io"))
	}
	var a, b bytes.Buffer
	NewLogger(&a, "info", testKey).Info("x", "user", "me@x.io")
	NewLogger(&b, "info", append([]byte(nil), testKey...)).Info("x", "user", "ME@X.IO")
	if !strings.Contains(a.String(), AddressTag(testKey, "me@x.io")) || !strings.Contains(b.String(), AddressTag(testKey, "me@x.io")) {
		t.Errorf("one address, two loggers with one key, two tags:\n%s%s", a.String(), b.String())
	}
	if AddressTag(testKey, "me@x.io") == AddressTag([]byte("another key, another install...."), "me@x.io") ||
		AddressTag(testKey, "me@x.io") == AddressTag(testKey, "you@x.io") {
		t.Error("tags collide across keys or addresses")
	}
}

// The key is made once, readable by its owner alone, and the same on every later start.
func TestLogKey(t *testing.T) {
	dir := t.TempDir() + "/data"
	if _, err := ReadLogKey(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadLogKey before any = %v, want not exist", err)
	}
	first, err := LogKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LogKey(dir)
	if err != nil || !bytes.Equal(first, again) || len(first) != 32 {
		t.Fatalf("second LogKey = %x, %v; first %x", again, err, first)
	}
	info, err := os.Stat(LogKeyFile(dir))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("log.key: %v, %v", info, err)
	}
	if err := os.WriteFile(LogKeyFile(dir), []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LogKey(dir); err == nil || !strings.Contains(err.Error(), "not a base64 key") {
		t.Errorf("a bad log.key: %v", err)
	}
}
