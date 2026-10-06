package telemetry

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerRedacts(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "debug")
	log.Debug("login", "user", "me@icloud.com", "password", "hunter2", "Token", "abc", "body", "Dear me",
		slog.Group("imap", "secret", "s3cr3t", "host", "imap.mail.me.com"))
	out := buf.String()
	for _, leak := range []string{"hunter2", "abc", "Dear me", "s3cr3t"} {
		if strings.Contains(out, leak) {
			t.Errorf("log leaked %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"me@icloud.com", "imap.mail.me.com"} {
		if !strings.Contains(out, keep) {
			t.Errorf("log dropped %q: %s", keep, out)
		}
	}
}

func TestLoggerLevel(t *testing.T) {
	var buf bytes.Buffer
	NewLogger(&buf, "warn").Info("quiet")
	if buf.Len() != 0 {
		t.Errorf("info logged at warn level: %s", buf.String())
	}
}
