package telemetry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

// render writes v so tests can compare it: maps and slices in order, scalars as Emit does.
func render(v attribute.Value) string {
	switch v.Type() {
	case attribute.MAP:
		var parts []string
		for _, kv := range v.AsMap() {
			parts = append(parts, string(kv.Key)+":"+render(kv.Value))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case attribute.SLICE:
		var parts []string
		for _, e := range v.AsSlice() {
			parts = append(parts, render(e))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case attribute.EMPTY:
		return "<none>"
	default:
		return fmt.Sprintf("%s(%s)", v.Type(), v.String())
	}
}

func attrsOf(r interface {
	WalkAttributes(func(attribute.KeyValue) bool)
}) string {
	var parts []string
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		parts = append(parts, string(kv.Key)+"="+render(kv.Value))
		return true
	})
	return strings.Join(parts, " ")
}

func TestLogRecord(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		severity otellog.Severity
		text     string
		body     string
		time     string
		attrs    string
	}{
		{"info with attributes", `{"time":"2026-10-13T10:00:00.123456789Z","level":"INFO","msg":"listening","listen":"127.0.0.1:8080","accounts":3,"dry_run":true}` + "\n",
			otellog.SeverityInfo, "INFO", "listening", "2026-10-13T10:00:00.123456789Z", "accounts=INT64(3) dry_run=BOOL(true) listen=STRING(127.0.0.1:8080)"},
		{"debug", `{"level":"DEBUG","msg":"d"}`, otellog.SeverityDebug, "DEBUG", "d", "", ""},
		{"warn", `{"level":"WARN","msg":"w"}`, otellog.SeverityWarn, "WARN", "w", "", ""},
		{"error with a group, a float and a null", `{"level":"ERROR","msg":"e","req":{"route":"/api/x","took":1.5},"tags":["a",2],"none":null}`,
			otellog.SeverityError, "ERROR", "e", "", "none=<none> req={route:STRING(/api/x),took:FLOAT64(1.5)} tags=[STRING(a),INT64(2)]"},
		{"between levels", `{"level":"ERROR+2","msg":"x"}`, otellog.SeverityError3, "ERROR+2", "x", "", ""},
		{"far below debug", `{"level":"DEBUG-20","msg":"x"}`, otellog.SeverityTrace1, "DEBUG-20", "x", "", ""},
		{"redacted address kept as written", `{"level":"INFO","msg":"sorted for <email:3f9a2c1b>"}`, otellog.SeverityInfo, "INFO", "sorted for <email:3f9a2c1b>", "", ""},
		{"not JSON", "panic: boom\n", otellog.SeverityUndefined, "", "panic: boom", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := logRecord([]byte(tt.line))
			if r.Severity() != tt.severity || r.SeverityText() != tt.text || r.Body().AsString() != tt.body {
				t.Errorf("severity %v %q, body %q; want %v %q, %q", r.Severity(), r.SeverityText(), r.Body().AsString(), tt.severity, tt.text, tt.body)
			}
			if tt.time != "" && r.Timestamp().Format(time.RFC3339Nano) != tt.time {
				t.Errorf("time %v, want %s", r.Timestamp(), tt.time)
			}
			if got := attrsOf(&r); got != tt.attrs {
				t.Errorf("attributes %q, want %q", got, tt.attrs)
			}
		})
	}
}

// fakeLogs is a log exporter that keeps what it is handed, or fails.
type fakeLogs struct {
	mu      sync.Mutex
	got     []sdklog.Record
	fail    error
	calls   int
	stopped bool
}

func (f *fakeLogs) Export(_ context.Context, recs []sdklog.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return f.fail
	}
	for _, r := range recs {
		f.got = append(f.got, r.Clone())
	}
	return nil
}
func (f *fakeLogs) Shutdown(context.Context) error   { f.stopped = true; return nil }
func (f *fakeLogs) ForceFlush(context.Context) error { return nil }

func testExport(t *testing.T, logs sdklog.Exporter) (*Export, *Metrics) {
	t.Helper()
	m := NewMetrics("1.2.3")
	x, err := newExport(t.Context(), ExportConfig{ServiceName: "mailrules-test", Version: "1.2.3", Environment: "staging"}, m, logs, sdkmetric.NewManualReader())
	if err != nil {
		t.Fatal(err)
	}
	return x, m
}

// run runs x until stop is called, which waits for it to have sent what is left.
func run(x *Export) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); x.Run(ctx) }()
	return func() { cancel(); <-done }
}

// The log, written through the export, reaches the exporter line for line, redacted as
// the log is, with the resource that names the service and the deployment.
func TestExportSendsLogLines(t *testing.T) {
	logs := &fakeLogs{}
	x, _ := testExport(t, logs)
	stop := run(x)
	var stderr bytes.Buffer
	l := NewLogger(io.MultiWriter(&stderr, x), "info", []byte("0123456789abcdef0123456789abcdef"))
	l.Info("sorted", "from", "someone@example.com", "password", "hunter2")
	l.Debug("not logged")
	l.Error("failed", "account", 7)
	stop()

	if !logs.stopped || len(logs.got) != 2 {
		t.Fatalf("exported %d records (stopped %v), want 2", len(logs.got), logs.stopped)
	}
	first := logs.got[0]
	attrs := attrsOf(&first)
	if first.Body().AsString() != "sorted" || first.Severity() != otellog.SeverityInfo || strings.Contains(attrs, "example.com") || strings.Contains(attrs, "hunter2") || !strings.Contains(attrs, "<email:") {
		t.Errorf("first record: %q %v %s", first.Body().AsString(), first.Severity(), attrs)
	}
	if !strings.Contains(stderr.String(), attrs[strings.Index(attrs, "<email:"):][:len("<email:3f9a2c1b>")]) {
		t.Errorf("the exported tag is not the logged one: %s vs %s", attrs, stderr.String())
	}
	if logs.got[1].Severity() != otellog.SeverityError || first.ObservedTimestamp().IsZero() {
		t.Errorf("second record severity %v, observed %v", logs.got[1].Severity(), first.ObservedTimestamp())
	}
	res := first.Resource().Set()
	for k, want := range map[attribute.Key]string{"service.name": "mailrules-test", "service.version": "1.2.3", "deployment.environment.name": "staging"} {
		if v, ok := res.Value(k); !ok || v.AsString() != want {
			t.Errorf("resource %s = %q, want %q", k, v.AsString(), want)
		}
	}
	if v, ok := res.Value("service.instance.id"); !ok || v.AsString() == "" {
		t.Error("resource has no service.instance.id")
	}
}

// Without an environment, deployment.environment.name is not sent at all.
func TestExportWithoutEnvironment(t *testing.T) {
	logs := &fakeLogs{}
	x, err := newExport(t.Context(), ExportConfig{ServiceName: "mailrules", Version: "dev"}, NewMetrics("dev"), logs, sdkmetric.NewManualReader())
	if err != nil {
		t.Fatal(err)
	}
	stop := run(x)
	_, _ = x.Write([]byte(`{"level":"INFO","msg":"hi"}`))
	stop()
	if len(logs.got) != 1 {
		t.Fatalf("exported %d records", len(logs.got))
	}
	if _, ok := logs.got[0].Resource().Set().Value("deployment.environment.name"); ok {
		t.Error("deployment.environment.name sent without MAILRULES_ENVIRONMENT")
	}
}

// Write never waits: past logQueue waiting lines it drops and counts them.
func TestExportQueueFull(t *testing.T) {
	logs := &fakeLogs{}
	x, m := testExport(t, logs)
	for range logQueue + 3 { // Run is not running: nothing is taken from the queue
		if n, err := x.Write([]byte(`{"level":"INFO","msg":"x"}`)); err != nil || n == 0 {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got := counter(t, m, "mailrules_otel_log_records_dropped_total", "queue_full"); got != 3 {
		t.Errorf("queue_full drops = %v, want 3", got)
	}
	stop := run(x)
	stop()
	if len(logs.got) != logQueue {
		t.Errorf("exported %d records, want %d", len(logs.got), logQueue)
	}
}

// A receiver that fails costs the batch, counted, and one warning until it works again.
func TestExportFailureCounted(t *testing.T) {
	logs := &fakeLogs{fail: errors.New("503 Service Unavailable")}
	x, m := testExport(t, logs)
	var logged bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logged, nil)))
	defer slog.SetDefault(old)

	ctx := t.Context()
	for i := range 3 {
		x.add(ctx, fmt.Appendf(nil, `{"level":"INFO","msg":"line %d"}`, i))
	}
	x.send(ctx)
	x.add(ctx, []byte(`{"level":"INFO","msg":"again"}`))
	x.send(ctx)
	if got := counter(t, m, "mailrules_otel_log_records_dropped_total", "export_failed"); got != 4 {
		t.Errorf("export_failed drops = %v, want 4", got)
	}
	if n := strings.Count(logged.String(), "could not export log lines"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, logged.String())
	}
	logs.mu.Lock()
	logs.fail = nil
	logs.mu.Unlock()
	x.add(ctx, []byte(`{"level":"INFO","msg":"back"}`))
	x.send(ctx)
	if len(logs.got) != 1 || !strings.Contains(logged.String(), "exported to OpenTelemetry again") {
		t.Errorf("after recovery: %d records, log:\n%s", len(logs.got), logged.String())
	}
}

// A batch is sent as soon as it is full, without waiting for the tick.
func TestExportSendsFullBatch(t *testing.T) {
	logs := &fakeLogs{}
	x, _ := testExport(t, logs)
	for range logBatch {
		x.add(t.Context(), []byte(`{"level":"INFO","msg":"x"}`))
	}
	if logs.calls != 1 || len(logs.got) != logBatch || len(x.batch.records) != 0 {
		t.Errorf("calls %d, exported %d, waiting %d", logs.calls, len(logs.got), len(x.batch.records))
	}
}

func counter(t *testing.T, m *Metrics, name, label string) float64 {
	t.Helper()
	fams, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, mt := range f.GetMetric() {
			for _, l := range mt.GetLabel() {
				if l.GetValue() == label {
					return mt.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// receiver is an OTLP/HTTP receiver: it keeps what it is sent, by path.
type receiver struct {
	mu      sync.Mutex
	logs    []*collogs.ExportLogsServiceRequest
	metrics []*colmetrics.ExportMetricsServiceRequest
	headers []http.Header
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil || r.Header.Get("Content-Type") != "application/x-protobuf" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.headers = append(rc.headers, r.Header.Clone())
	switch r.URL.Path {
	case "/base/v1/logs":
		req := &collogs.ExportLogsServiceRequest{}
		if proto.Unmarshal(body, req) != nil {
			http.Error(w, "bad logs", http.StatusBadRequest)
			return
		}
		rc.logs = append(rc.logs, req)
		out, _ := proto.Marshal(&collogs.ExportLogsServiceResponse{})
		_, _ = w.Write(out)
	case "/base/v1/metrics":
		req := &colmetrics.ExportMetricsServiceRequest{}
		if proto.Unmarshal(body, req) != nil {
			http.Error(w, "bad metrics", http.StatusBadRequest)
			return
		}
		rc.metrics = append(rc.metrics, req)
		out, _ := proto.Marshal(&colmetrics.ExportMetricsServiceResponse{})
		_, _ = w.Write(out)
	default:
		http.NotFound(w, r)
	}
}

func resourceAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// The real exporters send OTLP/HTTP protobuf to the endpoint's /v1/logs and /v1/metrics,
// with the headers: the log lines, and the Prometheus registry's metrics (the daemon's
// and the Go runtime's).
func TestExportOverHTTP(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	m := NewMetrics("1.2.3")
	m.ObserveHTTP("GET", "/api/settings", 200, time.Millisecond)
	x, err := NewExport(t.Context(), ExportConfig{Endpoint: srv.URL + "/base", Headers: map[string]string{"X-Obs-Key": "tok3n"},
		ServiceName: "mailrules-cloud", Version: "1.2.3", Environment: "production"}, m)
	if err != nil {
		t.Fatal(err)
	}
	stop := run(x)
	_, _ = x.Write([]byte(`{"time":"2026-10-13T10:00:00Z","level":"WARN","msg":"slow mailbox","account":4}` + "\n"))
	stop() // sends the lines left and the metrics' last reading

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if len(rc.logs) != 1 || len(rc.metrics) == 0 {
		t.Fatalf("received %d log and %d metric exports", len(rc.logs), len(rc.metrics))
	}
	for _, h := range rc.headers {
		if h.Get("X-Obs-Key") != "tok3n" {
			t.Errorf("export without the header: %v", h)
		}
	}
	rl := rc.logs[0].GetResourceLogs()[0]
	if got := resourceAttr(rl.GetResource().GetAttributes(), "deployment.environment.name"); got != "production" {
		t.Errorf("deployment.environment.name = %q", got)
	}
	rec := rl.GetScopeLogs()[0].GetLogRecords()[0]
	if rec.GetBody().GetStringValue() != "slow mailbox" || rec.GetSeverityText() != "WARN" || rec.GetAttributes()[0].GetValue().GetIntValue() != 4 {
		t.Errorf("log record: %v", rec)
	}
	names := map[string]bool{}
	for _, req := range rc.metrics {
		for _, rm := range req.GetResourceMetrics() {
			if got := resourceAttr(rm.GetResource().GetAttributes(), "service.name"); got != "mailrules-cloud" {
				t.Errorf("service.name = %q", got)
			}
			for _, sm := range rm.GetScopeMetrics() {
				for _, mt := range sm.GetMetrics() {
					names[mt.GetName()] = true
				}
			}
		}
	}
	for _, want := range []string{"mailrules_build_info", "mailrules_http_requests_total", "mailrules_http_request_duration_seconds", "go_goroutines"} {
		if !names[want] {
			t.Errorf("metric %s not sent; sent %v", want, names)
		}
	}
}
