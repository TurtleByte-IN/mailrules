package daemon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// log-tag prints the tag the log writes for each address, under the data directory's
// key, and makes no key where there is none.
func TestLogTag(t *testing.T) {
	dir := t.TempDir()
	getenv := func(k string) string {
		if k == "MAILRULES_DATA_DIR" {
			return dir
		}
		return ""
	}
	var out bytes.Buffer
	if err := logTag([]string{"me@x.io"}, getenv, &out); err == nil || !strings.Contains(err.Error(), "never redacted") {
		t.Errorf("without log.key: %v", err)
	}
	if _, err := telemetry.ReadLogKey(dir); err == nil {
		t.Error("log-tag made a key")
	}
	if err := logTag(nil, getenv, &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no address: %v", err)
	}
	key, err := telemetry.LogKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := logTag([]string{"Me@X.io", "you@y.io"}, getenv, &out); err != nil {
		t.Fatal(err)
	}
	want := telemetry.AddressTag(key, "me@x.io") + " Me@X.io\n" + telemetry.AddressTag(key, "you@y.io") + " you@y.io\n"
	if out.String() != want {
		t.Errorf("got %q, want %q", out.String(), want)
	}
}
