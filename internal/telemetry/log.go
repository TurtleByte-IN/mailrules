// Package telemetry sets up logging and metrics.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
)

// redacted lists attribute keys that are never written, at any level.
var redacted = map[string]bool{"password": true, "secret": true, "token": true, "key": true, "authorization": true, "body": true}

// redact is the logger's ReplaceAttr: it drops the attributes named in redacted, in groups too.
func redact(_ []string, a slog.Attr) slog.Attr {
	if redacted[strings.ToLower(a.Key)] {
		return slog.Attr{}
	}
	return a
}

// NewLogger returns a JSON logger that writes every record at level and above to w, and
// drops credentials and email bodies. With addressKey, every email address in a line, in
// its message as in its attributes, is written as its AddressTag under that key instead;
// nil keeps addresses. Each of errs gets, as well, every record at error level and above,
// as the one JSON line w gets for it (whatever level is): the same redaction, so it never
// holds more than the log does. A writer in errs must not block: the code that logged
// waits for it.
//
// A record logged with a context that carries attributes (WithAttrs, AddAttrs) has them
// too, except those whose key the record has already.
func NewLogger(w io.Writer, level string, addressKey []byte, errs ...io.Writer) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level)) // config.Validate already checked it; zero value is info
	out := func(w io.Writer) io.Writer {
		if addressKey == nil {
			return w
		}
		return addressWriter{w: w, key: addressKey}
	}
	h := handler{log: slog.NewJSONHandler(out(w), &slog.HandlerOptions{Level: lv, ReplaceAttr: redact})}
	for _, e := range errs {
		h.errs = append(h.errs, slog.NewJSONHandler(out(e), &slog.HandlerOptions{Level: slog.LevelError, ReplaceAttr: redact}))
	}
	return slog.New(h)
}

// handler writes each record to the log and, at error level, to the error writers.
type handler struct {
	log  slog.Handler
	errs []slog.Handler
}

func (h handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.log.Enabled(ctx, l) || (len(h.errs) > 0 && l >= slog.LevelError)
}

func (h handler) Handle(ctx context.Context, r slog.Record) error {
	r = withScope(ctx, r)
	var err error
	if h.log.Enabled(ctx, r.Level) {
		err = h.log.Handle(ctx, r)
	}
	if r.Level >= slog.LevelError {
		for _, e := range h.errs {
			err = errors.Join(err, e.Handle(ctx, r))
		}
	}
	return err
}

func (h handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h.with(func(x slog.Handler) slog.Handler { return x.WithAttrs(attrs) })
}

func (h handler) WithGroup(name string) slog.Handler {
	return h.with(func(x slog.Handler) slog.Handler { return x.WithGroup(name) })
}

func (h handler) with(f func(slog.Handler) slog.Handler) handler {
	out := handler{log: f(h.log), errs: make([]slog.Handler, len(h.errs))}
	for i, e := range h.errs {
		out.errs[i] = f(e)
	}
	return out
}

// scope is the attributes a context adds to what is logged with it: its own and those of
// the scopes around it, which its own override.
type scope struct {
	parent *scope
	mu     sync.Mutex
	attrs  []slog.Attr
}

type scopeKey struct{}

// WithAttrs returns ctx with attrs added to every record logged with it (slog.InfoContext
// and the like, through NewLogger's logger), such as the mailbox or the request route an
// error happened in. They override attributes of the same key that ctx carries already.
func WithAttrs(ctx context.Context, attrs ...slog.Attr) context.Context {
	parent, _ := ctx.Value(scopeKey{}).(*scope)
	return context.WithValue(ctx, scopeKey{}, &scope{parent: parent, attrs: slices.Clone(attrs)})
}

// AddAttrs adds attrs to the attributes ctx carries (WithAttrs) in place, so that what is
// logged with the context WithAttrs returned has them as well: how a request learns its
// tenant once the session is known, for what its outer layers log about it. It does
// nothing when ctx carries none.
func AddAttrs(ctx context.Context, attrs ...slog.Attr) {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return
	}
	s.mu.Lock()
	s.attrs = append(s.attrs, attrs...)
	s.mu.Unlock()
}

// withScope adds the attributes ctx carries to r, but not those of a key r has.
func withScope(ctx context.Context, r slog.Record) slog.Record {
	s, _ := ctx.Value(scopeKey{}).(*scope)
	if s == nil {
		return r
	}
	seen := map[string]bool{}
	r.Attrs(func(a slog.Attr) bool {
		seen[a.Key] = true
		return true
	})
	var add []slog.Attr
	for ; s != nil; s = s.parent {
		s.mu.Lock()
		for _, a := range slices.Backward(s.attrs) { // the latest of a key wins
			if !seen[a.Key] {
				seen[a.Key] = true
				add = append(add, a)
			}
		}
		s.mu.Unlock()
	}
	if len(add) == 0 {
		return r
	}
	slices.Reverse(add)
	r = r.Clone() // the caller's record may share its attributes' storage
	r.AddAttrs(add...)
	return r
}

// LogPanic logs v, what recover returned, at error level under msg, with args, the panic
// as "panic" and the stack of the goroutine that panicked as "stack". Call it from the
// deferred function that recovered.
func LogPanic(ctx context.Context, msg string, v any, args ...any) {
	slog.ErrorContext(ctx, msg, append(args, "panic", fmt.Sprint(v), "stack", string(debug.Stack()))...)
}
