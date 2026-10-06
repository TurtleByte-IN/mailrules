// Package telemetry sets up logging (and, later, metrics).
package telemetry

import (
	"io"
	"log/slog"
	"strings"
)

// redacted lists attribute keys that are never written, at any level.
var redacted = map[string]bool{"password": true, "secret": true, "token": true, "key": true, "authorization": true, "body": true}

// NewLogger returns a JSON logger that drops credentials and email bodies.
func NewLogger(w io.Writer, level string) *slog.Logger {
	var lv slog.Level
	_ = lv.UnmarshalText([]byte(level)) // config.Validate already checked it; zero value is info
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if redacted[strings.ToLower(a.Key)] {
				return slog.Attr{}
			}
			return a
		},
	}))
}
