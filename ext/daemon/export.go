package daemon

import (
	"context"
	"io"
	"os"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// startExport starts the OpenTelemetry export of the metrics and the log when
// OTEL_EXPORTER_OTLP_ENDPOINT is set (MAI-239). It returns where the log is to be written,
// standard error and, when the export is on, the export too, and stop, which ends the
// export once what is waiting has been sent (or exportDrainTime has passed). Without an
// endpoint it starts nothing: no goroutine, no connection.
func startExport(ctx context.Context, cfg *config.Config, version string, metrics *telemetry.Metrics) (io.Writer, func(), error) {
	if cfg.OTLPEndpoint == "" {
		return os.Stderr, func() {}, nil
	}
	headers, err := cfg.OTLPHeaderMap()
	if err != nil {
		return nil, nil, err
	}
	x, err := telemetry.NewExport(ctx, telemetry.ExportConfig{
		Endpoint: cfg.OTLPEndpoint, Headers: headers,
		ServiceName: cfg.ServiceName, Version: version, Environment: cfg.Environment,
	}, metrics)
	if err != nil {
		return nil, nil, err
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	done := make(chan struct{})
	go func() {
		defer close(done)
		x.Run(runCtx)
	}()
	return io.MultiWriter(os.Stderr, x), func() { cancel(); <-done }, nil
}
