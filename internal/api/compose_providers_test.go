package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProvider is an OpenAI-compatible or Ollama server that answers every chat with the
// drafts, in its own wire format, and counts the calls.
func fakeProvider(t *testing.T, kind string, answer string) (url string, calls *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n++
		w.Header().Set("Content-Type", "application/json")
		quoted, _ := json.Marshal(answer)
		switch {
		case kind == "openai" && r.URL.Path == "/chat/completions":
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"test-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"answer","arguments":`+
				string(quoted)+`}}]}}],"usage":{"prompt_tokens":900,"completion_tokens":400,"total_tokens":1300}}`)
		case kind == "ollama" && r.URL.Path == "/api/chat":
			_, _ = io.WriteString(w, `{"model":"test","message":{"role":"assistant","content":`+string(quoted)+`},"done":true,"prompt_eval_count":900,"eval_count":400}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// Describe it runs on whichever provider composer_model names; without what that provider
// needs, the 409 says what to add for it.
func TestComposeOnOtherProviders(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings func(url string) string // the PATCH that chooses the provider, without its key or URL
		complete func(url string) string // the PATCH that adds it
		missing  string
	}{
		{
			name: "openai",
			settings: func(url string) string {
				return `{"composer_model":"openai:test-model","openai_base_url":"` + url + `"}`
			},
			complete: func(string) string { return `{"keys":{"openai_api_key":"sk-test-openai"}}` },
			missing:  "This needs an AI model. The rule composer model, openai:test-model, needs an OpenAI API key: add it in Settings, then try again. Rules built from conditions work without one.",
		},
		{
			name:     "ollama",
			settings: func(string) string { return `{"composer_model":"ollama:test"}` },
			complete: func(url string) string { return `{"ollama_url":"` + url + `"}` },
			missing:  "This needs an AI model. The rule composer model, ollama:test, needs the URL of your Ollama server: add it in Settings, then try again. Rules built from conditions work without one.",
		},
		{
			name:     "claude",
			settings: func(string) string { return `{"composer_model":"claude-haiku-4-5"}` },
			missing:  "This needs an AI model. The rule composer model, claude-haiku-4-5, needs a Claude (Anthropic) API key: add it in Settings, then try again. Rules built from conditions work without one.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.realGen = true
			e.deliver("noreply@swiggy.in", "order one")
			e.connect()
			url, calls := fakeProvider(t, tc.name, fiveDrafts)
			e.call(http.MethodPatch, "/api/settings", tc.settings(url), http.StatusOK)

			r := e.do(http.MethodPost, "/api/rules/compose", `{"text":"Put Swiggy in Food","account_id":1}`)
			if r.status != http.StatusConflict || r.body.Error.Code != "no_composer_model" || r.body.Error.Message != tc.missing {
				t.Fatalf("composing without the provider's key or URL = %d %s %q", r.status, r.body.Error.Code, r.body.Error.Message)
			}
			if tc.complete == nil {
				return
			}
			e.call(http.MethodPatch, "/api/settings", tc.complete(url), http.StatusOK)
			got := e.call(http.MethodPost, "/api/rules/compose", `{"text":"`+paragraph+`"}`, http.StatusOK)
			drafts := got["rules"].([]any)
			conform(t, e.doc, "ComposeResult", got)
			if len(drafts) != 5 || drafts[0].(map[string]any)["name"] != "Food" || *calls != 1 {
				t.Fatalf("%d drafts after %d provider calls: %v", len(drafts), *calls, got)
			}
			usage := e.call(http.MethodGet, "/api/stats/usage", "", http.StatusOK)
			if !strings.Contains(string(mustJSON(t, usage)), `"provider":"`+tc.name+`"`) {
				t.Errorf("the call is not on the ledger under %s: %v", tc.name, usage)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
