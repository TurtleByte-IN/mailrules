package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"maps"
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

// Errors reach the error writers exactly as the log writes them, redaction included;
// records below error level never do, whatever the log's level.
func TestLoggerErrorWriters(t *testing.T) {
	var logged, errs bytes.Buffer
	log := NewLogger(&logged, "debug", &errs)
	log.Debug("quiet")
	log.Info("quiet")
	log.Warn("quiet")
	if errs.Len() != 0 {
		t.Fatalf("below error reached the error writer: %s", errs.String())
	}
	logged.Reset()
	log.With("password", "hunter2", "account", 7).Error("failed", "token", "abc", "Authorization", "Bearer x", "body", "Dear me",
		slog.Group("imap", "secret", "s3cr3t", "host", "imap.mail.me.com"), "error", "refused")
	if errs.String() != logged.String() {
		t.Errorf("error writer got\n%s\nthe log\n%s", errs.String(), logged.String())
	}
	for _, leak := range []string{"hunter2", "abc", "Bearer", "Dear me", "s3cr3t"} {
		if strings.Contains(errs.String(), leak) {
			t.Errorf("error writer got %q: %s", leak, errs.String())
		}
	}
	for _, keep := range []string{`"account":7`, "imap.mail.me.com", `"error":"refused"`, `"msg":"failed"`} {
		if !strings.Contains(errs.String(), keep) {
			t.Errorf("error writer lacks %s: %s", keep, errs.String())
		}
	}

	// A log that writes only errors still hands each over; one that writes none still does.
	for _, level := range []string{"error", "error+4"} {
		logged.Reset()
		errs.Reset()
		NewLogger(&logged, level, &errs).Error("failed")
		if !strings.Contains(errs.String(), `"msg":"failed"`) {
			t.Errorf("level %s: error writer got %q", level, errs.String())
		}
	}
}

// What a context carries is logged with every record logged with it, the record's own
// attributes and the inner context's winning; added in place, it reaches what was logged
// with the outer context.
func TestLoggerContextAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(&buf, "info")
	line := func(ctx context.Context, args ...any) map[string]any {
		t.Helper()
		buf.Reset()
		log.InfoContext(ctx, "x", args...)
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		delete(m, "time")
		return m
	}
	outer := WithAttrs(t.Context(), slog.String("route", "GET /api/rules"), slog.Int("account", 1))
	inner := WithAttrs(outer, slog.Int("account", 2))
	AddAttrs(inner, slog.Int("tenant_id", 9))
	AddAttrs(t.Context(), slog.Int("ignored", 1)) // no attributes to add to

	for _, c := range []struct {
		ctx  context.Context
		args []any
		want map[string]any
	}{
		{t.Context(), nil, map[string]any{"level": "INFO", "msg": "x"}},
		{outer, nil, map[string]any{"level": "INFO", "msg": "x", "route": "GET /api/rules", "account": 1.0}},
		{inner, nil, map[string]any{"level": "INFO", "msg": "x", "route": "GET /api/rules", "account": 2.0, "tenant_id": 9.0}},
		{inner, []any{"account", 3}, map[string]any{"level": "INFO", "msg": "x", "route": "GET /api/rules", "account": 3.0, "tenant_id": 9.0}},
	} {
		if got := line(c.ctx, c.args...); !maps.Equal(got, c.want) {
			t.Errorf("logged %v, want %v", got, c.want)
		}
	}
}

func panics() { panic("boom") }

// A recovered panic is logged at error with its value and the stack of where it happened.
func TestLogPanic(t *testing.T) {
	var logged, errs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(NewLogger(&logged, "info", &errs))
	t.Cleanup(func() { slog.SetDefault(old) })
	func() {
		defer func() { LogPanic(t.Context(), "it panicked", recover(), "account", 4) }()
		panics()
	}()
	var m map[string]any
	if err := json.Unmarshal(errs.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	stack, _ := m["stack"].(string)
	if m["level"] != "ERROR" || m["msg"] != "it panicked" || m["panic"] != "boom" || m["account"] != 4.0 ||
		!strings.Contains(stack, "telemetry.panics(") {
		t.Errorf("logged %v", m)
	}
}
