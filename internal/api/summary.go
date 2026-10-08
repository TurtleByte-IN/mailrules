package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/settings"
	"github.com/TurtleByte-IN/mailrules/internal/store"
	"github.com/TurtleByte-IN/mailrules/internal/summary"
)

// summaryJSON is the summary email's part of GET /api/settings (SummarySettings).
func (s *server) summaryJSON(ctx context.Context, u store.User) (map[string]any, error) {
	v, err := s.Summary.View(ctx, u)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"enabled": v.Enabled, "frequency": v.Frequency, "weekday": v.Weekday, "time": v.Time, "time_zone": v.TimeZone,
		"to": v.To, "to_default": v.ToDefault, "smtp": map[string]any{"configured": v.Configured, "missing": v.Missing},
		"last_sent_at": ts(v.LastSentAt), "next_at": ts(v.NextAt),
	}, nil
}

// summaryRefused answers for a summary change or send that cannot be made, and reports
// whether err was one.
func summaryRefused(w http.ResponseWriter, err error) bool {
	var bad *settings.Invalid
	var noSMTP *summary.NoSMTP
	switch {
	case errors.As(err, &bad):
		invalid(w, bad.Path, bad.Message)
	case errors.As(err, &noSMTP):
		writeError(w, http.StatusConflict, "smtp_not_configured", noSMTP.Message(), "summary.enabled")
	default:
		return false
	}
	return true
}

// handleSummaryPreview renders the next summary without sending it.
func (s *server) handleSummaryPreview(w http.ResponseWriter, r *http.Request) {
	email, c, to, err := s.Summary.Preview(r.Context(), user(r))
	if err != nil {
		internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"to": to, "subject": email.Subject, "text": email.Text, "html": email.HTML,
		"period_start": c.Start.Unix(), "period_end": c.End.Unix(), "dry_run": c.DryRun})
}

// previewPolicy lets the preview document style itself with the inline styles every email
// carries, which the app's own policy forbids, and nothing else: no script, no request
// out, framed only by the app, and sandboxed even when opened on its own.
const previewPolicy = "default-src 'none'; style-src 'unsafe-inline'; img-src data:; base-uri 'none'; form-action 'none'; " +
	"frame-ancestors 'self'; sandbox allow-popups allow-popups-to-escape-sandbox"

// handleSummaryPreviewPage is the preview's HTML as a document of its own, for the Settings
// card's frame: a srcdoc frame would inherit the app's policy and lose the email's styles.
// Its links open in a new tab.
func (s *server) handleSummaryPreviewPage(w http.ResponseWriter, r *http.Request) {
	email, _, _, err := s.Summary.Preview(r.Context(), user(r))
	if err != nil {
		internalError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Security-Policy", previewPolicy)
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(strings.Replace(email.HTML, "<head>", `<head><base target="_blank">`, 1)))
}

// handleSummaryTest sends a summary of the last day now.
func (s *server) handleSummaryTest(w http.ResponseWriter, r *http.Request) {
	email, to, err := s.Summary.SendTest(r.Context(), user(r))
	var notSent *summary.SendError
	switch {
	case summaryRefused(w, err):
	case errors.As(err, &notSent):
		if clientGone(w, r) {
			return
		}
		// What the mail server said is for the admin who set it up; it holds no secret.
		writeError(w, http.StatusBadGateway, "send_failed", "The summary email could not be sent: "+notSent.Err.Error()+".", "")
	case err != nil:
		internalError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"to": to, "subject": email.Subject})
	}
}
