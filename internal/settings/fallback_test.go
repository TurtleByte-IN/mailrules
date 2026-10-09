package settings

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs sends slog to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &logs
}

// fallbackLines are the log lines about the fallback model.
func fallbackLines(logs *bytes.Buffer) []string {
	var out []string
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(l, "fallback model") {
			out = append(out, l)
		}
	}
	return out
}

// A fallback that is set but cannot be asked is reported truthfully: once in the log when
// the settings are built, and by Settings as inactive with the reason. A fallback that
// works, or one the user turned off, is never reported.
func TestFallbackNotActiveIsReported(t *testing.T) {
	const orKey = "sk-or-test-secret"
	tests := []struct {
		name       string
		env        map[string]string
		off        bool // the user saved an empty fallback model
		wantActive bool
		wantNote   string // "" = none; otherwise a part of it
		wantLog    bool
	}{
		{"no Claude key", map[string]string{"OPENROUTER_API_KEY": orKey}, false, false, "needs a Claude (Anthropic) API key", true},
		{"Claude key set", map[string]string{"OPENROUTER_API_KEY": orKey, "ANTHROPIC_API_KEY": "sk-ant-test-secret"}, false, true, "", false},
		{"turned off", map[string]string{"OPENROUTER_API_KEY": orKey}, true, false, "", false},
		{"the decider is the fallback", map[string]string{"MAILRULES_DECIDER": "anthropic", "ANTHROPIC_API_KEY": "sk-ant-test-secret"}, false, false, "the decision model already is claude-haiku-4-5", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			s := newSettings(t, tt.env)
			if tt.off {
				off := ""
				if err := s.Apply(t.Context(), 1, Patch{FallbackModel: &off}); err != nil {
					t.Fatal(err)
				}
			}
			v := view(t, s)
			if v.FallbackActive != tt.wantActive || (tt.wantNote == "") != (v.FallbackNote == "") || !strings.Contains(v.FallbackNote, tt.wantNote) {
				t.Errorf("view: active=%v note=%q, want active=%v note containing %q", v.FallbackActive, v.FallbackNote, tt.wantActive, tt.wantNote)
			}
			// Startup builds the router, and so does every email; the warning is one line, once.
			for range 3 {
				s.Live(t.Context(), 1)
			}
			lines := fallbackLines(logs)
			if want := map[bool]int{true: 1, false: 0}[tt.wantLog]; len(lines) != want {
				t.Fatalf("fallback log lines = %q, want %d", lines, want)
			}
			if tt.wantLog && (!strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "claude-haiku-4-5") || !strings.Contains(lines[0], tt.wantNote)) {
				t.Errorf("line = %s", lines[0])
			}
			if strings.Contains(logs.String(), "secret") {
				t.Errorf("a key reached the log: %s", logs.String())
			}
		})
	}
}

// Saving a Claude key clears the note and the next rebuild logs nothing; removing it brings
// both back, once more.
func TestFallbackReportFollowsTheKey(t *testing.T) {
	logs := captureLogs(t)
	s := newSettings(t, map[string]string{"OPENROUTER_API_KEY": "sk-or-test-secret"})
	s.Live(t.Context(), 1)
	if len(fallbackLines(logs)) != 1 || view(t, s).FallbackActive {
		t.Fatalf("before: log %q, view %+v", fallbackLines(logs), view(t, s))
	}
	if err := s.Apply(t.Context(), 1, Patch{Keys: map[string]string{"anthropic_api_key": "sk-ant-test-secret"}}); err != nil {
		t.Fatal(err)
	}
	s.Live(t.Context(), 1)
	if v := view(t, s); !v.FallbackActive || v.FallbackNote != "" || len(fallbackLines(logs)) != 1 {
		t.Fatalf("with a key: view active=%v note=%q, log %q", v.FallbackActive, v.FallbackNote, fallbackLines(logs))
	}
	if err := s.Apply(t.Context(), 1, Patch{Keys: map[string]string{"anthropic_api_key": ""}}); err != nil {
		t.Fatal(err)
	}
	s.Live(t.Context(), 1)
	if v := view(t, s); v.FallbackActive || v.FallbackNote == "" || len(fallbackLines(logs)) != 2 {
		t.Fatalf("key removed: view active=%v note=%q, log %q", v.FallbackActive, v.FallbackNote, fallbackLines(logs))
	}
}
