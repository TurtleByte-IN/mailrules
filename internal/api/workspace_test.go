package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/models/anthropictest"
)

// fakeFinder answers workspace lookups as the test sets it and counts them.
type fakeFinder struct {
	answer models.WorkspaceAnswer
	err    error
	calls  atomic.Int32
}

func (f *fakeFinder) Find(context.Context, string) (models.WorkspaceAnswer, error) {
	f.calls.Add(1)
	return f.answer, f.err
}

// The Claude workspace in Settings: looked up when a key is saved, given again without
// asking once known, chosen or typed by the user, refused when it is not a workspace ID.
func TestAnthropicWorkspaceSettings(t *testing.T) {
	e := newEnv(t)
	e.signIn()
	finder := &fakeFinder{answer: models.WorkspaceAnswer{Workspaces: []models.Workspace{{ID: "wrkspc_default", Name: "Default"}}}}
	e.sett.Workspaces = finder
	lookup := func(status string, n int) {
		t.Helper()
		got := e.call(http.MethodGet, "/api/settings/anthropic-workspaces", "", http.StatusOK)
		conform(t, e.doc, "AnthropicWorkspaces", got)
		if got["status"] != status || len(got["workspaces"].([]any)) != n {
			t.Errorf("lookup = %v, want %s with %d", got, status, n)
		}
	}
	workspace := func(s map[string]any, id, name string, found bool) {
		t.Helper()
		conform(t, e.doc, "Settings", s)
		if s["anthropic_workspace_id"] != id || s["anthropic_workspace_name"] != name || s["anthropic_workspace_found"] != found {
			t.Errorf("settings say %v %v found %v, want %q %q found %v", s["anthropic_workspace_id"], s["anthropic_workspace_name"],
				s["anthropic_workspace_found"], id, name, found)
		}
	}

	// No Claude key: nothing to name, and Anthropic is not asked.
	lookup("none_needed", 0)
	workspace(e.call(http.MethodGet, "/api/settings", "", http.StatusOK), "", "", false)

	// A key whose organisation has one workspace: saving it stores the workspace.
	workspace(e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"sk-ant-one"}}`, http.StatusOK), "wrkspc_default", "Default", true)
	lookup("one", 1)
	if finder.calls.Load() != 1 {
		t.Errorf("asked Anthropic %d times, want once", finder.calls.Load())
	}

	// Several: nothing is stored, the list is given again without asking.
	finder.answer.Workspaces = append(finder.answer.Workspaces, models.Workspace{ID: "wrkspc_mail", Name: "Mail"})
	workspace(e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"sk-ant-several"}}`, http.StatusOK), "", "", false)
	lookup("several", 2)
	if finder.calls.Load() != 2 {
		t.Errorf("asked Anthropic %d times, want twice", finder.calls.Load())
	}
	e.refuse(http.MethodPatch, "/api/settings", `{"anthropic_workspace_id":"Mail"}`, http.StatusBadRequest, "invalid_input", "anthropic_workspace_id")
	workspace(e.call(http.MethodPatch, "/api/settings", `{"anthropic_workspace_id":"wrkspc_mail"}`, http.StatusOK), "wrkspc_mail", "Mail", false)
	workspace(e.call(http.MethodPatch, "/api/settings", `{"anthropic_workspace_id":null}`, http.StatusOK), "", "", false)

	// Anthropic cannot say: failed, and asked again next time.
	finder.err = errors.New("anthropic: HTTP 403 Forbidden: permission_error")
	e.call(http.MethodPatch, "/api/settings", `{"keys":{"anthropic_api_key":"sk-ant-nolist"}}`, http.StatusOK)
	lookup("failed", 0)
	if finder.calls.Load() != 4 {
		t.Errorf("asked Anthropic %d times, want 4", finder.calls.Load())
	}
	workspace(e.call(http.MethodPatch, "/api/settings", `{"anthropic_workspace_id":"wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ"}`, http.StatusOK), "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ", "", false)
}

// When Claude refuses the key for want of a workspace, Describe it, Rewrite with AI, Suggest
// from my mail and the rule tester answer 409 anthropic_workspace_needed, and a stream that
// had begun ends with an error event saying the same.
func TestAnthropicWorkspaceNeeded(t *testing.T) {
	e := newEnv(t)
	for i := range 4 {
		e.deliver("noreply@swiggy.in", fmt.Sprintf("order %d", i))
		e.deliver("hello@news.example", fmt.Sprintf("Reading issue %d", i))
	}
	e.connect()
	e.call(http.MethodPost, "/api/rules/import", rulesYAML, http.StatusOK)
	refused := fmt.Errorf("primary decider anthropic: %w", &models.StatusError{Provider: "anthropic", Code: http.StatusBadRequest,
		Type: "invalid_request_error", Detail: anthropictest.WorkspaceNeeded})
	e.gen = &models.Fake{GenerateFunc: func(string, string, json.RawMessage, any) (models.Usage, error) { return models.Usage{}, refused }}
	e.decider.DecideFunc = func(models.DecideRequest) (models.Decision, models.Usage, error) {
		return models.Decision{}, models.Usage{}, refused
	}
	const want = "Your Claude key covers your whole organisation, so MailRules needs to know which workspace to use. Choose it in Settings."

	for _, c := range []struct{ feature, path, body string }{
		{"Describe it", "/api/rules/compose", `{"text":"Put Swiggy in Food"}`},
		{"Rewrite with AI", "/api/rules/1/compose", `{"text":"and Zomato"}`},
		{"Suggest from my mail", "/api/rules/suggest", `{"account_id":1}`},
		{"the rule tester", "/api/rules/test", `{"account_id":1,"limit":5}`},
	} {
		r := e.do(http.MethodPost, c.path, c.body)
		if r.status != http.StatusConflict || r.body.Error.Code != "anthropic_workspace_needed" || r.body.Error.Message != want {
			t.Errorf("%s = %d %s %q", c.feature, r.status, r.body.Error.Code, r.body.Error.Message)
		}
	}
	for _, c := range []struct{ feature, path, body string }{
		{"Suggest from my mail", "/api/rules/suggest", `{"account_id":1}`},
		{"the rule tester", "/api/rules/test", `{"account_id":1,"limit":5}`},
	} {
		r := e.sse(c.path, c.body)
		names, data := streamEvents(t, r.raw)
		last := len(names) - 1
		if r.header.Get("Content-Type") != "text/event-stream" || names[last] != "error" {
			t.Fatalf("%s stream = %s, events %v", c.feature, r.header.Get("Content-Type"), names)
		}
		conform(t, e.doc, "ErrorBody", data[last])
		if got := data[last]["error"].(map[string]any); got["code"] != "anthropic_workspace_needed" || got["message"] != want {
			t.Errorf("%s error event = %v", c.feature, got)
		}
	}
}
