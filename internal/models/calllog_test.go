package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a log destination the concurrent calls of a test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// debugLog makes the daemon's logger write JSON at debug level into the returned buffer
// until the test ends.
func debugLog(t *testing.T) *syncBuffer {
	t.Helper()
	buf, old := &syncBuffer{}, slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return buf
}

// modelCalls are the "model call" lines in a log.
func modelCalls(t *testing.T, log string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(log), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("not a log line: %q", line)
		}
		if v["msg"] == "model call" {
			out = append(out, v)
		}
	}
	return out
}

func TestOutcomeClasses(t *testing.T) {
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want string
	}{
		{"answered", t.Context(), nil, "ok"},
		{"rate limited", t.Context(), &StatusError{Provider: "p", Code: 429}, "rate_limited"},
		{"server error", t.Context(), &StatusError{Provider: "p", Code: 503}, "http_503"},
		{"refused key", t.Context(), &StatusError{Provider: "p", Code: 401}, "http_401"},
		{"a status inside a wrapped error", t.Context(), fmt.Errorf("decide: %w", &StatusError{Provider: "p", Code: 500}), "http_500"},
		{"attempt ran out of time", t.Context(), fmt.Errorf("p: %w", context.DeadlineExceeded), "timeout"},
		{"caller gave up", cancelled, context.Canceled, "cancelled"},
		{"caller gave up on an attempt that also timed out", cancelled, context.DeadlineExceeded, "cancelled"},
		{"connection refused", t.Context(), errors.New("p: dial tcp: connection refused"), "other"},
		{"unusable answer", t.Context(), fmt.Errorf("p: %w", ErrBadOutput), "other"},
	} {
		if got := outcomeOf(c.ctx, c.err); got != c.want {
			t.Errorf("%s: outcome = %q, want %q", c.name, got, c.want)
		}
	}
}

// Every attempt of a model call writes one debug line with who, why, which attempt, how
// long and how it ended; a retried call writes one per attempt.
func TestCallerWritesOneDebugLinePerAttempt(t *testing.T) {
	t.Run("retries", func(t *testing.T) {
		log := debugLog(t)
		url, _ := fakeProvider(t, reply{http.StatusTooManyRequests, `{}`}, reply{http.StatusBadGateway, `{}`}, reply{http.StatusOK, adapters[1].ok})
		r := (&Router{Primary: adapters[1].build(url, testDeps())}).For("test")
		if _, err := r.Route(t.Context(), testRequest); err != nil {
			t.Fatal(err)
		}
		calls := modelCalls(t, log.String())
		if len(calls) != 3 {
			t.Fatalf("%d lines, want one per attempt:\n%s", len(calls), log.String())
		}
		for i, want := range []string{"rate_limited", "http_502", "ok"} {
			c := calls[i]
			if c["level"] != "DEBUG" || c["provider"] != "cloudflare" || c["model"] != "@cf/cloudflare/clef-flash" || c["purpose"] != "test" ||
				c["attempt"] != float64(i+1) || c["outcome"] != want {
				t.Errorf("attempt %d = %v, want outcome %s", i+1, c, want)
			}
			if _, ok := c["duration_ms"].(float64); !ok {
				t.Errorf("attempt %d has no duration_ms: %v", i+1, c)
			}
		}
	})

	t.Run("a timeout", func(t *testing.T) {
		log := debugLog(t)
		c := NewCaller(1)
		c.Timeout, c.Retries = 5*time.Millisecond, 0
		err := c.Do(t.Context(), Call{"anthropic", "claude-haiku-4-5"}, func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
		calls := modelCalls(t, log.String())
		if !errors.Is(err, context.DeadlineExceeded) || len(calls) != 1 || calls[0]["outcome"] != "timeout" || calls[0]["purpose"] != "other" {
			t.Errorf("err %v, lines %v", err, calls)
		}
	})

	t.Run("the purpose follows the router", func(t *testing.T) {
		log := debugLog(t)
		url, _ := fakeProvider(t, reply{http.StatusOK, adapters[1].ok})
		r := &Router{Primary: adapters[1].build(url, testDeps())}
		if _, err := r.Route(t.Context(), testRequest); err != nil {
			t.Fatal(err)
		}
		if calls := modelCalls(t, log.String()); len(calls) != 1 || calls[0]["purpose"] != "decide" {
			t.Errorf("lines = %v", calls)
		}
	})
}

// Whatever a call is about (the key, the email, the model's answer), none of it is in the
// debug lines of any adapter, though the call itself was made with all of it.
func TestModelCallLogsNeverCarryContent(t *testing.T) {
	for _, a := range adapters {
		t.Run(a.name, func(t *testing.T) {
			log := debugLog(t)
			url, got := fakeProvider(t, reply{http.StatusInternalServerError, `{"error":{"type":"server_error","message":"` + testEmail.Subject + `"}}`}, reply{http.StatusOK, a.ok})
			if _, err := (&Router{Primary: a.build(url, testDeps())}).For("test").Route(t.Context(), testRequest); err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(got.body, []byte(testEmail.Subject)) {
				t.Fatal("the provider was not sent the email, so this proves nothing")
			}
			calls := modelCalls(t, log.String())
			if len(calls) != 2 || calls[0]["provider"] != a.wantUsage.Provider || calls[0]["model"] != a.wantUsage.Model || calls[1]["outcome"] != "ok" {
				t.Fatalf("lines = %v", calls)
			}
			for _, secretText := range []string{secret, testEmail.Subject, testEmail.From, testEmail.Body, "Food delivery order updates", "tiffinwheel"} {
				if strings.Contains(log.String(), secretText) {
					t.Errorf("the log contains %q:\n%s", secretText, log.String())
				}
			}
		})
	}
}
