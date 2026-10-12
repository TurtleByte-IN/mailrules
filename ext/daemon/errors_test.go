package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// receive waits for the next event a module was handed.
func receive(t *testing.T, got <-chan ext.ErrorEvent) ext.ErrorEvent {
	t.Helper()
	select {
	case e := <-got:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("the module was handed nothing")
		return ext.ErrorEvent{}
	}
}

// startSink runs the sink of the one module of modules that takes errors, behind the
// daemon's logger, until the test ends.
func startSink(t *testing.T, metrics *telemetry.Metrics, modules ...ext.Module) (*slog.Logger, context.CancelFunc, chan struct{}) {
	t.Helper()
	sinks := errorSinks(modules, "v1.2.3", metrics)
	if len(sinks) != 1 {
		t.Fatalf("%d sinks, want 1", len(sinks))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sinks[0].Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-done })
	return telemetry.NewLogger(io.Discard, "debug", sinks[0]), cancel, done
}

// The module is handed each error as the log line reads, redacted, with the build's version
// and the stack apart; nothing below error level reaches it.
func TestErrorsHandsTheLoggedLine(t *testing.T) {
	got := make(chan ext.ErrorEvent, 4)
	log, _, _ := startSink(t, nil, ext.Module{Name: "plain"},
		ext.Module{Name: "tracker", Errors: func(_ context.Context, e ext.ErrorEvent) { got <- e }})

	ctx := telemetry.WithAttrs(t.Context(), slog.String("route", "GET /api/rules"))
	log.DebugContext(ctx, "quiet")
	log.InfoContext(ctx, "quiet")
	log.WarnContext(ctx, "quiet")
	before := time.Now()
	log.With("tenant_id", 3).ErrorContext(ctx, "failed", "password", "hunter2", "token", "abc", "body", "Dear me",
		slog.Group("imap", "key", "k", "host", "imap.mail.me.com"), "error", "refused", "stack", "goroutine 1 [running]")
	e := receive(t, got)
	if e.Message != "failed" || e.Version != "v1.2.3" || e.Stack != "goroutine 1 [running]" || e.Dropped != 0 ||
		e.Time.Before(before.Truncate(time.Millisecond)) || time.Since(e.Time) > time.Minute {
		t.Errorf("event = %+v", e)
	}
	want := map[string]any{"tenant_id": json.Number("3"), "route": "GET /api/rules", "error": "refused",
		"imap": map[string]any{"host": "imap.mail.me.com"}}
	if b, w := mustJSON(t, e.Attrs), mustJSON(t, want); b != w {
		t.Errorf("attrs = %s, want %s", b, w)
	}
	select {
	case e := <-got:
		t.Errorf("handed more: %+v", e)
	default:
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A module that does not return never holds up logging: errors beyond ErrorQueue drop the
// oldest waiting, counted in the metric and in the next event handed; once it returns, it
// gets the newest ErrorQueue in order.
func TestErrorsNeverBlockLogging(t *testing.T) {
	got, release := make(chan ext.ErrorEvent, 2*ext.ErrorQueue), make(chan struct{})
	metrics := telemetry.NewMetrics("v1.2.3")
	log, _, _ := startSink(t, metrics, ext.Module{Name: "tracker", Errors: func(_ context.Context, e ext.ErrorEvent) {
		got <- e
		<-release
	}})
	log.Error("first")
	if e := receive(t, got); e.Message != "first" {
		t.Fatalf("first event = %+v", e)
	}
	// The module is stuck in the first; these all return at once.
	const extra = 50
	logged := make(chan struct{})
	go func() {
		defer close(logged)
		for i := range ext.ErrorQueue + extra {
			log.Error("more", "i", i)
		}
	}()
	select {
	case <-logged:
	case <-time.After(5 * time.Second):
		t.Fatal("logging waited for the module")
	}
	close(release)
	for i := range ext.ErrorQueue {
		e := receive(t, got)
		wantDropped := 0
		if i == 0 {
			wantDropped = extra
		}
		if e.Attrs["i"] != json.Number(strconv.Itoa(extra+i)) || e.Dropped != wantDropped {
			t.Fatalf("event %d = i %v, dropped %d; want i %d, dropped %d", i, e.Attrs["i"], e.Dropped, extra+i, wantDropped)
		}
	}
	rec := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if want := `mailrules_error_events_dropped_total{module="tracker"} 50`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("metrics lack %s", want)
	}
}

// Errors still waiting when the daemon stops are handed over, with a ctx that outlives the
// stop by a few seconds.
func TestErrorsDrainedOnStop(t *testing.T) {
	got, release := make(chan ext.ErrorEvent, 8), make(chan struct{})
	log, cancel, done := startSink(t, nil, ext.Module{Name: "tracker", Errors: func(ctx context.Context, e ext.ErrorEvent) {
		if e.Message == "first" {
			<-release
		}
		if ctx.Err() != nil {
			e.Message += ": ctx ended"
		}
		got <- e
	}})
	log.Error("first")
	log.Error("second")
	log.Error("third")
	cancel()
	close(release)
	<-done
	for _, want := range []string{"first", "second", "third"} {
		if e := receive(t, got); e.Message != want {
			t.Errorf("handed %q, want %q", e.Message, want)
		}
	}
}

// End to end: a request that panics is answered 500 and the daemon goes on; the module is
// handed the panic with its stack, the request's route, method and tenant, and the errors
// the request logged before it, redacted. A request that panics once its answer has begun
// is cut off.
func TestErrorsFromTheDaemon(t *testing.T) {
	var (
		mu     sync.Mutex
		events []ext.ErrorEvent
	)
	mod := ext.Module{Name: "tracker",
		Errors: func(_ context.Context, e ext.ErrorEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		},
		Routes: func(ext.Host) []ext.Route {
			return []ext.Route{
				{Method: http.MethodGet, Path: "/api/tracker/panic", Handler: func(_ http.ResponseWriter, r *http.Request) {
					slog.InfoContext(r.Context(), "about to fail")
					slog.ErrorContext(r.Context(), "it failed", "password", "hunter2", "error", "refused")
					panic("boom")
				}},
				{Method: http.MethodGet, Path: "/api/tracker/cut", Handler: func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte("partial"))
					_ = http.NewResponseController(w).Flush()
					panic("cut")
				}},
			}
		}}
	d := serveForTest(t, nil, mod)
	_, body := d.c.do(http.MethodGet, "/api/tracker/panic", nil, http.StatusInternalServerError)
	if !strings.Contains(string(body), `"code":"internal"`) {
		t.Errorf("answer = %s", body)
	}
	d.c.do(http.MethodGet, "/api/auth/me", nil, http.StatusOK) // the daemon goes on

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, d.c.base+"/api/tracker/cut", nil)
	resp, err := d.c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err == nil || string(b) != "partial" {
		t.Errorf("cut-off answer read %q, %v; want partial and an error", b, err)
	}

	var failed, boom, cut ext.ErrorEvent
	eventually(t, "the module is handed the errors", func() (bool, string) {
		mu.Lock()
		defer mu.Unlock()
		for _, e := range events {
			switch {
			case e.Message == "it failed":
				failed = e
			case e.Attrs["panic"] == "boom":
				boom = e
			case e.Attrs["panic"] == "cut":
				cut = e
			}
		}
		return boom.Message != "" && cut.Message != "" && failed.Message != "", mustJSON(t, events)
	})
	tenant := json.Number(strconv.FormatInt(store.SelfHostTenant, 10))
	if boom.Message != "a request panicked" || boom.Version != "test" || !strings.Contains(boom.Stack, "TestErrorsFromTheDaemon") ||
		boom.Attrs["route"] != "GET /api/tracker/panic" || boom.Attrs["method"] != "GET" || boom.Attrs["tenant_id"] != tenant {
		t.Errorf("panic event = %+v", boom)
	}
	if failed.Stack != "" || failed.Attrs["password"] != nil || failed.Attrs["error"] != "refused" ||
		failed.Attrs["route"] != "GET /api/tracker/panic" || failed.Attrs["tenant_id"] != tenant {
		t.Errorf("error event = %+v", failed)
	}
	if cut.Stack == "" || cut.Attrs["route"] != "GET /api/tracker/cut" {
		t.Errorf("cut-off event = %+v", cut)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range events {
		if e.Message == "about to fail" {
			t.Errorf("handed an info line: %+v", e)
		}
	}
}
