package telemetry

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/go-logr/logr"
	otelprom "go.opentelemetry.io/contrib/bridges/prometheus"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// The log export's limits. A line waits in a queue of logQueue (one more is dropped and
// counted); lines are sent logBatch at a time, or every logEvery when fewer are waiting.
// When the daemon stops, what is still waiting, and the metrics' last reading, get
// exportDrainTime to be sent.
const (
	logQueue        = 2048
	logBatch        = 512
	logEvery        = time.Second
	exportDrainTime = 5 * time.Second
)

// ExportConfig says where the OpenTelemetry export sends, and as what.
type ExportConfig struct {
	// Endpoint is the receiver's base URL: metrics go to /v1/metrics under it, log lines
	// to /v1/logs (OTLP/HTTP, protobuf).
	Endpoint string
	// Headers go with every export; they hold the receiver's key, so they are never logged.
	Headers     map[string]string
	ServiceName string
	Version     string
	// Environment is deployment.environment.name; empty sends none.
	Environment string
}

// Export sends the daemon's metrics and log to an OpenTelemetry receiver (MAI-239). The
// metrics are the Prometheus registry's (Metrics), read and sent by the SDK's periodic
// reader; the log lines are those the log writes, redaction included, handed to Write.
// Nothing about it ever waits on the receiver: Write only queues, and a receiver that is
// slow or down costs dropped lines, counted in mailrules_otel_log_records_dropped_total.
type Export struct {
	metrics *Metrics
	lines   chan []byte
	logs    sdklog.Exporter
	logger  otellog.Logger
	batch   *batch
	meters  *sdkmetric.MeterProvider
	// failing is whether the last log export failed: a failure is logged once, not once a
	// batch, so a receiver that is down does not fill the log (and the export) with it.
	failing  bool
	draining bool
}

// NewExport starts the metrics' periodic export and returns the Export whose Run sends the
// log lines Write queues. It connects to nothing before the first export.
func NewExport(ctx context.Context, cfg ExportConfig, m *Metrics) (*Export, error) {
	base, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return nil, errors.New("OTEL_EXPORTER_OTLP_ENDPOINT is not a URL") // never quoted: it may hold a password
	}
	// The SDK's own messages can quote what it read from the environment, headers included:
	// they are dropped. Its errors (a failed metrics export) are logged, as warnings.
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("could not export to OpenTelemetry", "error", err.Error())
	}))
	logs, err := otlploghttp.New(ctx, otlploghttp.WithEndpointURL(base.JoinPath("v1", "logs").String()), otlploghttp.WithHeaders(cfg.Headers))
	if err != nil {
		return nil, fmt.Errorf("OpenTelemetry log exporter: %w", err)
	}
	meters, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(base.JoinPath("v1", "metrics").String()), otlpmetrichttp.WithHeaders(cfg.Headers))
	if err != nil {
		return nil, fmt.Errorf("OpenTelemetry metric exporter: %w", err)
	}
	return newExport(ctx, cfg, m, logs, sdkmetric.NewPeriodicReader(meters, sdkmetric.WithProducer(otelprom.NewMetricProducer(otelprom.WithGatherer(m.reg)))))
}

// newExport is NewExport with the exporters given: tests hand it fakes.
func newExport(ctx context.Context, cfg ExportConfig, m *Metrics, logs sdklog.Exporter, reader sdkmetric.Reader) (*Export, error) {
	attrs := []attribute.KeyValue{semconv.ServiceName(cfg.ServiceName), semconv.ServiceVersion(cfg.Version), semconv.ServiceInstanceID(rand.Text())}
	if cfg.Environment != "" {
		attrs = append(attrs, semconv.DeploymentEnvironmentNameKey.String(cfg.Environment))
	}
	res, err := resource.New(ctx, resource.WithSchemaURL(semconv.SchemaURL), resource.WithTelemetrySDK(), resource.WithAttributes(attrs...))
	if err != nil {
		return nil, fmt.Errorf("OpenTelemetry resource: %w", err)
	}
	b := &batch{}
	return &Export{
		metrics: m,
		lines:   make(chan []byte, logQueue),
		logs:    logs,
		logger:  sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(b)).Logger("github.com/TurtleByte-IN/mailrules"),
		batch:   b,
		meters:  sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(reader)),
	}, nil
}

// Write queues a copy of the log line p. It never blocks and never fails: with logQueue
// lines waiting, p is dropped and counted.
func (x *Export) Write(p []byte) (int, error) {
	select {
	case x.lines <- bytes.Clone(p): // the logger reuses p
	default:
		x.metrics.droppedLogs("queue_full", 1)
	}
	return len(p), nil
}

// Run sends the queued log lines until ctx is done, then, for up to exportDrainTime, the
// metrics' last reading and the lines still queued. Lines written after it returns are
// never sent.
func (x *Export) Run(ctx context.Context) {
	tick := time.NewTicker(logEvery)
	defer tick.Stop()
	for {
		select {
		case line := <-x.lines:
			x.add(ctx, line)
		case <-tick.C:
			x.send(ctx)
		case <-ctx.Done():
			x.drain(ctx)
			return
		}
	}
}

// drain sends what is left when the daemon stops.
func (x *Export) drain(ctx context.Context) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportDrainTime)
	defer cancel()
	x.draining = true
	if err := x.meters.Shutdown(ctx); err != nil {
		slog.Warn("could not export the last metrics to OpenTelemetry", "error", err.Error())
	}
	for len(x.lines) > 0 { // only Run receives, so this never blocks
		x.add(ctx, <-x.lines)
	}
	x.send(ctx)
	_ = x.logs.Shutdown(ctx)
}

// add turns line into a log record, and sends the batch once it holds logBatch.
func (x *Export) add(ctx context.Context, line []byte) {
	x.logger.Emit(ctx, logRecord(line))
	if len(x.batch.records) >= logBatch {
		x.send(ctx)
	}
}

// send exports the batched records. A batch that cannot be sent is dropped and counted,
// except one cut short by the daemon stopping: drain sends it again.
func (x *Export) send(ctx context.Context) {
	recs := x.batch.records
	if len(recs) == 0 {
		return
	}
	err := x.logs.Export(ctx, recs)
	if err != nil && ctx.Err() != nil && !x.draining {
		return
	}
	clear(recs)
	x.batch.records = recs[:0]
	switch {
	case err != nil:
		x.metrics.droppedLogs("export_failed", len(recs))
		if !x.failing {
			slog.Warn("could not export log lines to OpenTelemetry; they are dropped, and counted in mailrules_otel_log_records_dropped_total, until it works again", "error", err.Error(), "dropped", len(recs))
		}
		x.failing = true
	case x.failing:
		x.failing = false
		slog.Info("log lines are exported to OpenTelemetry again")
	}
}

// batch is the log provider's one processor: it keeps each record emitted, for send. Only
// Run's goroutine emits, so it needs no lock.
type batch struct{ records []sdklog.Record }

func (b *batch) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
func (b *batch) OnEmit(_ context.Context, r *sdklog.Record) error {
	b.records = append(b.records, r.Clone())
	return nil
}
func (b *batch) Shutdown(context.Context) error   { return nil }
func (b *batch) ForceFlush(context.Context) error { return nil }

// logRecord turns one JSON log line into a log record: its time, its level as severity,
// its message as body and every other key as an attribute (a group as a map). A line that
// is not a JSON object is sent whole, as the body.
func logRecord(line []byte) otellog.Record {
	var r otellog.Record
	var fields map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&fields); err != nil {
		r.SetBody(attribute.StringValue(string(bytes.TrimSpace(line))))
		return r
	}
	if s, ok := fields[slog.TimeKey].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			r.SetTimestamp(t)
		}
	}
	if s, ok := fields[slog.LevelKey].(string); ok {
		var l slog.Level
		if l.UnmarshalText([]byte(s)) == nil {
			// slog's levels are 4 apart from DEBUG (-4) to ERROR (8); OpenTelemetry's are 4
			// apart from DEBUG (5) to ERROR (17).
			r.SetSeverity(otellog.Severity(min(max(int(l)+int(otellog.SeverityInfo), int(otellog.SeverityTrace1)), int(otellog.SeverityFatal4))))
		}
		r.SetSeverityText(s)
	}
	msg, _ := fields[slog.MessageKey].(string)
	r.SetBody(attribute.StringValue(msg))
	delete(fields, slog.TimeKey)
	delete(fields, slog.LevelKey)
	delete(fields, slog.MessageKey)
	r.AddAttributes(keyValues(fields)...)
	return r
}

// keyValues turns decoded JSON fields into attributes, in key order.
func keyValues(fields map[string]any) []attribute.KeyValue {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	kvs := make([]attribute.KeyValue, len(keys))
	for i, k := range keys {
		kvs[i] = attribute.KeyValue{Key: attribute.Key(k), Value: logValue(fields[k])}
	}
	return kvs
}

// logValue turns one decoded JSON value into a log value.
func logValue(v any) attribute.Value {
	switch v := v.(type) {
	case string:
		return attribute.StringValue(v)
	case bool:
		return attribute.BoolValue(v)
	case json.Number:
		if n, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			return attribute.Int64Value(n)
		}
		if f, err := v.Float64(); err == nil {
			return attribute.Float64Value(f)
		}
		return attribute.StringValue(string(v))
	case map[string]any:
		return attribute.MapValue(keyValues(v)...)
	case []any:
		vs := make([]attribute.Value, len(v))
		for i, e := range v {
			vs[i] = logValue(e)
		}
		return attribute.SliceValue(vs...)
	default: // null
		return attribute.Value{}
	}
}
