package daemon

import (
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"sync"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// Without OTEL_EXPORTER_OTLP_ENDPOINT the log goes to standard error alone, and nothing
// is started: no goroutine, so no connection either.
func TestStartExportOff(t *testing.T) {
	cfg, err := config.Load(nil, func(k string) string {
		return map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "X-Obs-Key=tok3n", "MAILRULES_ENVIRONMENT": "staging"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	before := runtime.NumGoroutine()
	w, stop, err := startExport(t.Context(), cfg, "dev", telemetry.NewMetrics("dev"))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if w != os.Stderr {
		t.Errorf("log writer = %T, want os.Stderr", w)
	}
	if after := runtime.NumGoroutine(); after > before { // fewer: an earlier test's goroutine ended
		t.Errorf("goroutines %d -> %d", before, after)
	}
}

// With an endpoint, what is written to the log writer reaches the receiver, with the
// headers, by the time stop returns.
func TestStartExportOn(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{} // path -> X-Obs-Key
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("X-Obs-Key")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer srv.Close()
	cfg, err := config.Load(nil, func(k string) string {
		return map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, "OTEL_EXPORTER_OTLP_HEADERS": "X-Obs-Key=tok3n"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	w, stop, err := startExport(t.Context(), cfg, "dev", telemetry.NewMetrics("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(`{"level":"INFO","msg":"exported"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	if got["/v1/logs"] != "tok3n" || got["/v1/metrics"] != "tok3n" {
		t.Errorf("received %v, want logs and metrics with the header", got)
	}
}
