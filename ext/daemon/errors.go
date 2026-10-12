package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/telemetry"
)

// errorDrainTime is how long the errors still waiting for a module when the daemon stops
// are handed to it.
const errorDrainTime = 5 * time.Second

// errorSink hands one module (ext.Module.Errors) the error lines the log writes: it is one
// of telemetry.NewLogger's error writers. Write only queues a copy of the line, at most
// ext.ErrorQueue of them, dropping the oldest, so logging never waits for the module; Run
// hands them over one at a time.
type errorSink struct {
	module  string
	deliver func(context.Context, ext.ErrorEvent)
	version string
	metrics *telemetry.Metrics

	mu      sync.Mutex
	lines   [][]byte // a ring of ext.ErrorQueue lines: n of them from head on
	head, n int
	dropped int           // dropped since the last line taken
	ready   chan struct{} // has a value when lines may have been queued since Run last looked
}

// errorSinks returns a sink for each module that takes the daemon's errors.
func errorSinks(modules []ext.Module, version string, metrics *telemetry.Metrics) []*errorSink {
	var out []*errorSink
	for _, m := range modules {
		if m.Errors != nil {
			out = append(out, &errorSink{module: m.Name, deliver: m.Errors, version: version, metrics: metrics,
				lines: make([][]byte, ext.ErrorQueue), ready: make(chan struct{}, 1)})
		}
	}
	return out
}

// Write queues the line p. It never blocks and never fails.
func (s *errorSink) Write(p []byte) (int, error) {
	line := bytes.Clone(p) // the logger reuses p
	s.mu.Lock()
	if s.n == len(s.lines) {
		s.lines[s.head] = nil
		s.head, s.n = (s.head+1)%len(s.lines), s.n-1
		s.dropped++
		s.metrics.DroppedError(s.module)
	}
	s.lines[(s.head+s.n)%len(s.lines)] = line
	s.n++
	s.mu.Unlock()
	select {
	case s.ready <- struct{}{}:
	default:
	}
	return len(p), nil
}

// take returns the oldest queued line and how many were dropped before it.
func (s *errorSink) take() (line []byte, dropped int, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return nil, 0, false
	}
	line, s.lines[s.head] = s.lines[s.head], nil
	s.head, s.n = (s.head+1)%len(s.lines), s.n-1
	dropped, s.dropped = s.dropped, 0
	return line, dropped, true
}

// Run hands the queued lines to the module until ctx is done, then what is still queued
// for up to errorDrainTime. The module's ctx ends errorDrainTime after ctx does.
func (s *errorSink) Run(ctx context.Context) {
	mctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(errorDrainTime, cancel) })
	defer stop()
	for ctx.Err() == nil {
		select {
		case <-s.ready:
			s.handAll(mctx)
		case <-ctx.Done():
		}
	}
	s.handAll(mctx)
}

// handAll hands the queued lines to the module until none is left or ctx is done.
func (s *errorSink) handAll(ctx context.Context) {
	for ctx.Err() == nil {
		line, dropped, ok := s.take()
		if !ok {
			return
		}
		s.hand(ctx, line, dropped)
	}
}

// hand gives the module the event of one line.
func (s *errorSink) hand(ctx context.Context, line []byte, dropped int) {
	var attrs map[string]any
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	if err := dec.Decode(&attrs); err != nil {
		slog.WarnContext(ctx, "a logged error could not be read back for a module", "module", s.module, "error", err.Error())
		return
	}
	e := ext.ErrorEvent{Version: s.version, Dropped: dropped}
	e.Message, _ = attrs[slog.MessageKey].(string)
	e.Stack, _ = attrs["stack"].(string)
	if t, ok := attrs[slog.TimeKey].(string); ok {
		e.Time, _ = time.Parse(time.RFC3339Nano, t)
	}
	for _, k := range []string{slog.TimeKey, slog.LevelKey, slog.MessageKey, "stack"} {
		delete(attrs, k)
	}
	e.Attrs = attrs
	s.deliver(ctx, e)
}
