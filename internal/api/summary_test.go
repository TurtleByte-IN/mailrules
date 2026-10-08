package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/mailer"
)

func TestSummarySettings(t *testing.T) {
	e := newEnv(t)
	e.signIn()

	got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)
	conform(t, e.doc, "Settings", got)
	sum := got["summary"].(map[string]any)
	for key, want := range map[string]any{"enabled": false, "frequency": "daily", "weekday": "monday", "time": "08:00", "time_zone": "UTC",
		"to": "me@icloud.com", "to_default": "me@icloud.com", "last_sent_at": nil, "next_at": nil} {
		if sum[key] != want {
			t.Errorf("default summary.%s = %v, want %v", key, sum[key], want)
		}
	}
	if smtp := sum["smtp"].(map[string]any); smtp["configured"] != true || len(smtp["missing"].([]any)) != 0 {
		t.Errorf("smtp = %v", smtp)
	}
	if _, ok := got["features"].(map[string]any)["digest"]; ok {
		t.Error("the digest flag is still there")
	}

	got = e.call(http.MethodPatch, "/api/settings",
		`{"summary":{"enabled":true,"frequency":"weekly","weekday":"friday","time":"18:30","time_zone":"Europe/Berlin","to":"Neha <neha@example.com>"}}`, http.StatusOK)
	conform(t, e.doc, "Settings", got)
	sum = got["summary"].(map[string]any)
	if sum["enabled"] != true || sum["frequency"] != "weekly" || sum["weekday"] != "friday" || sum["time"] != "18:30" ||
		sum["time_zone"] != "Europe/Berlin" || sum["to"] != "neha@example.com" || sum["next_at"] == nil {
		t.Errorf("summary after the change = %v", sum)
	}
	// Fields left out stay; null puts the admin's address back.
	sum = e.call(http.MethodPatch, "/api/settings", `{"summary":{"to":null,"time_zone":"Europe/Berlin"}}`, http.StatusOK)["summary"].(map[string]any)
	if sum["to"] != "me@icloud.com" || sum["frequency"] != "weekly" || sum["enabled"] != true {
		t.Errorf("summary after to:null = %v", sum)
	}

	for body, path := range map[string]string{
		`{"summary":{"time":"25:00"}}`:                 "summary.time",
		`{"summary":{"frequency":"hourly"}}`:           "summary.frequency",
		`{"summary":{"weekday":"Friday"}}`:             "summary.weekday",
		`{"summary":{"time_zone":"Mars/Olympus"}}`:     "summary.time_zone",
		`{"summary":{"to":"not an address"}}`:          "summary.to",
		`{"summary":null}`:                             "summary",
		`{"summary":{"digest":true}}`:                  "summary",
		`{"summary":{"time":"25:00"},"dry_run":false}`: "summary.time",
	} {
		e.refuse(http.MethodPatch, "/api/settings", body, http.StatusBadRequest, "invalid_input", path)
	}
	if got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK); got["dry_run"] != true {
		t.Error("a refused summary change saved the other settings sent with it")
	}
}

func TestSummaryNeedsAMailServer(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	e.summary.Sender, e.summary.Missing = nil, []string{"MAILRULES_SMTP_HOST", "MAILRULES_SMTP_FROM"}

	sum := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["summary"].(map[string]any)
	if smtp := sum["smtp"].(map[string]any); smtp["configured"] != false || len(smtp["missing"].([]any)) != 2 {
		t.Errorf("smtp = %v", smtp)
	}
	r := e.do(http.MethodPatch, "/api/settings", `{"dry_run":false,"summary":{"enabled":true,"time_zone":"UTC"}}`)
	if r.status != http.StatusConflict || r.body.Error.Code != "smtp_not_configured" || r.body.Error.Path != "summary.enabled" ||
		!strings.Contains(r.body.Error.Message, "MAILRULES_SMTP_HOST and MAILRULES_SMTP_FROM") {
		t.Fatalf("switching on = %d %s", r.status, r.raw)
	}
	got := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)
	if got["dry_run"] != true || got["summary"].(map[string]any)["enabled"] != false {
		t.Error("the refused change saved something")
	}
	// The rest of the card still saves, and the preview works.
	e.call(http.MethodPatch, "/api/settings", `{"summary":{"time":"07:00","time_zone":"UTC"}}`, http.StatusOK)
	e.call(http.MethodGet, "/api/summary/preview", "", http.StatusOK)
	e.refuse(http.MethodPost, "/api/summary/test", "", http.StatusConflict, "smtp_not_configured", "summary.enabled")
	if n := e.mail.Tries(); n != 0 {
		t.Errorf("tried to send %d times", n)
	}
}

func TestSummaryTestAndPreview(t *testing.T) {
	e := newEnv(t)
	e.signIn()

	preview := e.call(http.MethodGet, "/api/summary/preview", "", http.StatusOK)
	conform(t, e.doc, "SummaryPreview", preview)

	// The frame's page: the email's HTML, under a policy that allows its inline styles only.
	page := e.do(http.MethodGet, "/api/summary/preview.html", "")
	if page.status != http.StatusOK || page.header.Get("Content-Type") != "text/html; charset=utf-8" ||
		!strings.Contains(string(page.raw), `<head><base target="_blank">`) || !strings.Contains(string(page.raw), "style=") {
		t.Fatalf("preview page = %d %s", page.status, page.raw)
	}
	csp := page.header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "style-src 'unsafe-inline'", "frame-ancestors 'self'", "sandbox allow-popups"} {
		if !strings.Contains(csp, want) {
			t.Errorf("policy %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "script") || page.header.Get("X-Frame-Options") != "SAMEORIGIN" {
		t.Errorf("policy %q, X-Frame-Options %q", csp, page.header.Get("X-Frame-Options"))
	}
	if preview["to"] != "me@icloud.com" || preview["subject"] != "MailRules dry-run: 0 would be sorted, nothing to review" || preview["dry_run"] != true ||
		!strings.Contains(preview["html"].(string), "<!DOCTYPE html>") || !strings.Contains(preview["text"].(string), "http://127.0.0.1:8080/#/settings") {
		t.Errorf("preview = %v", preview)
	}
	if n := e.mail.Tries(); n != 0 {
		t.Fatalf("the preview sent %d", n)
	}

	got := e.call(http.MethodPost, "/api/summary/test", "", http.StatusOK)
	conform(t, e.doc, "SummaryTestResult", got)
	sent := e.mail.Sent()
	if len(sent) != 1 || sent[0].To != "me@icloud.com" || got["to"] != "me@icloud.com" || got["subject"] != sent[0].Subject || sent[0].HTML == "" {
		t.Fatalf("test sent %+v, answered %v", sent, got)
	}

	e.mail.SetErr(errors.Join(mailer.ErrRefused, errors.New("550 5.1.1 no such user")))
	r := e.do(http.MethodPost, "/api/summary/test", "")
	if r.status != http.StatusBadGateway || r.body.Error.Code != "send_failed" || !strings.Contains(r.body.Error.Message, "550 5.1.1 no such user") {
		t.Fatalf("a refused test = %d %s", r.status, r.raw)
	}
}
