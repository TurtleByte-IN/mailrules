package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/TurtleByte-IN/mailrules/ext"
	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/models/anthropictest"
)

// The free build has no suggest module: GET /api/settings says so, and the route the
// contract documents for it is there, behind the sign-in, and refuses with not_available.
func TestFreeBuildRefusesSuggest(t *testing.T) {
	e := newEnv(t)
	e.call(http.MethodGet, "/api/auth/me", "", http.StatusUnauthorized) // hands out the CSRF cookie
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusUnauthorized, "setup_required", "")
	e.connect()
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["suggest"]; on != false {
		t.Errorf("features.suggest = %v without the module", on)
	}
	r := e.do(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`)
	if r.status != http.StatusNotFound || r.body.Error.Code != "not_available" || r.body.Error.Message != "Suggest from my mail is not included in this build of MailRules." {
		t.Fatalf("suggest in the free build = %d %s", r.status, r.raw)
	}
	conform(t, e.doc, "ErrorBody", r.object(t))
}

// probe is a module that uses every part of the host, the way a real one does: it reads a
// scope, lists and fetches the mail, reads the folders and rules, calls the composer model,
// books the call and answers as JSON or as a stream.
func probe() ext.Module {
	return ext.Module{Name: "suggest", Routes: func(h ext.Host) []ext.Route {
		serve := func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				ext.ScopeInput
				Mode string `json:"mode"`
			}
			if !h.ReadJSON(w, r, &in) {
				return
			}
			sc, ok := h.Scope(w, r, in.ScopeInput)
			if !ok {
				return
			}
			if in.Mode != "" {
				h.Invalid(w, "mode", "There are no modes.")
				return
			}
			ctx := r.Context()
			gen, err := h.Composer(ctx)
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			mb, err := h.Mail(sc.AccountID)
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			refs, matched, err := mb.List(ctx, sc.Folder, sc.Since, sc.Limit)
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			var mu sync.Mutex
			var subjects []string
			unread := 0
			err = mb.Fetch(ctx, refs, ext.FetchOptions{NoBody: true}, func(_ context.Context, _ int, sum *ext.Summary, seen bool) error {
				mu.Lock()
				defer mu.Unlock()
				if sum != nil && sum.Body == "" {
					subjects = append(subjects, sum.Subject)
				}
				if !seen {
					unread++
				}
				return nil
			})
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			folders, err := h.Folders(ctx, sc.AccountID)
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			rs, err := h.Rules(ctx, h.UserID(r))
			if err != nil {
				h.Fail(w, r, err)
				return
			}
			es := h.EventStream(w)
			if h.WantsStream(r) {
				es.Send("progress", map[string]int{"read": len(subjects)})
			}
			var out map[string]any
			u, err := gen.Generate(ext.WithPurpose(ctx, "suggest"), "system", "user", json.RawMessage(`{"type":"object"}`), &out)
			if err == nil || u.TokensIn > 0 {
				h.RecordUsage(ctx, "suggest", u)
				h.UsageChanged()
			}
			switch {
			case err != nil && es.Started():
				es.Fail(err, "suggest_failed", "The scan stopped.")
			case err != nil:
				h.Fail(w, r, fmt.Errorf("%w: %w", ext.ErrModel, err))
			case es.Started():
				es.Send("done", map[string]any{"emails": len(subjects)})
			default:
				h.WriteJSON(w, http.StatusOK, map[string]any{"emails": len(subjects), "unread": unread, "matched": matched, "folders": folders, "rules": len(rs)})
			}
		}
		return []ext.Route{{Method: http.MethodPost, Path: "/api/rules/suggest", Handler: serve}}
	}}
}

// A module's route replaces the refusal and is served like a built-in one: behind the
// sign-in and the CSRF check, with the host's helpers answering as the built-in routes do.
func TestModuleHost(t *testing.T) {
	e := newEnv(t, probe())
	for i := range 4 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
	}
	e.call(http.MethodGet, "/api/auth/me", "", http.StatusUnauthorized) // hands out the CSRF cookie
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusUnauthorized, "setup_required", "")
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	if on := e.call(http.MethodGet, "/api/settings", "", http.StatusOK)["features"].(map[string]any)["suggest"]; on != true {
		t.Errorf("features.suggest = %v with the module", on)
	}
	e.noCSRF = true
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusForbidden, "csrf_failed", "")
	e.noCSRF = false

	// Refusals in the built-in shapes.
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1,"nope":1}`, http.StatusBadRequest, "invalid_json", "")
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":9}`, http.StatusBadRequest, "invalid_input", "account_id")
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1,"limit":0}`, http.StatusBadRequest, "invalid_input", "limit")
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1,"mode":"x"}`, http.StatusBadRequest, "invalid_input", "mode")
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusConflict, "no_composer_model", "")

	e.gen = &models.Fake{GenerateFunc: func(string, string, json.RawMessage, any) (models.Usage, error) {
		return models.Usage{Provider: "anthropic", Model: "claude-haiku-4-5", TokensIn: 100, TokensOut: 20, CostUSD: 0.001}, nil
	}}
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1,"folder":"Nope"}`, http.StatusBadRequest, "invalid_input", "folder")
	got := e.call(http.MethodPost, "/api/rules/suggest", `{"account_id":1,"limit":3}`, http.StatusOK)
	if got["emails"] != float64(3) || got["unread"] != float64(3) || got["matched"] != float64(4) || got["rules"] != float64(2) ||
		!slices.Contains(got["folders"].([]any), any("INBOX")) {
		t.Errorf("probe = %v", got)
	}
	if n := e.count(`SELECT COALESCE(SUM(calls), 0) FROM usage_daily WHERE purpose = 'suggest'`); n != 1 {
		t.Errorf("%d suggest calls on the ledger, want 1", n)
	}
	// The mail was read with BODY.PEEK: nothing is marked read.
	refs, _, err := e.mb.FetchSince(t.Context(), "INBOX", time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if flags, _ := e.mb.Flags(t.Context(), ref); slices.Contains(flags, `\Seen`) {
			t.Errorf("email %d was marked read", ref.UID)
		}
	}

	// A model that fails: 502 model_error as plain JSON, the workspace sentence as JSON or
	// as the stream's error event.
	e.gen = &models.Fake{GenerateFunc: func(string, string, json.RawMessage, any) (models.Usage, error) {
		return models.Usage{}, &models.StatusError{Provider: "anthropic", Code: http.StatusServiceUnavailable}
	}}
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusBadGateway, "model_error", "")
	refused := fmt.Errorf("composer anthropic: %w", &models.StatusError{Provider: "anthropic", Code: http.StatusBadRequest,
		Type: "invalid_request_error", Detail: anthropictest.WorkspaceNeeded})
	e.gen = &models.Fake{GenerateFunc: func(string, string, json.RawMessage, any) (models.Usage, error) { return models.Usage{}, refused }}
	e.refuse(http.MethodPost, "/api/rules/suggest", `{"account_id":1}`, http.StatusConflict, "anthropic_workspace_needed", "")
	r := e.sse("/api/rules/suggest", `{"account_id":1}`)
	names, data := streamEvents(t, r.raw)
	if r.header.Get("Content-Type") != "text/event-stream" || !slices.Equal(names, []string{"progress", "error"}) {
		t.Fatalf("stream = %s, events %v", r.header.Get("Content-Type"), names)
	}
	conform(t, e.doc, "ErrorBody", data[1])
	if code := data[1]["error"].(map[string]any)["code"]; code != "anthropic_workspace_needed" {
		t.Errorf("error event code = %v", code)
	}
}
