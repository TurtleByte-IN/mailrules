package models

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A refusal says why in the log, without what the request said (MAI-56).
func TestErrorReplyShapes(t *testing.T) {
	for _, tt := range []struct {
		name, body, typ, msg string
	}{
		{"anthropic and openai", `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum"}}`,
			"invalid_request_error", "prompt is too long: 215000 tokens > 200000 maximum"},
		{"ollama", `{"error":"model not found, try pulling it first"}`, "", "model not found, try pulling it first"},
		{"cloudflare", `{"success":false,"errors":[{"code":5006,"message":"Bad input"}]}`, "", "Bad input"},
		{"not json", `<html>502 Bad Gateway</html>`, "", ""},
		{"json of another shape", `{"detail":"nope"}`, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if typ, msg := errorReply([]byte(tt.body)); typ != tt.typ || msg != tt.msg {
				t.Errorf("errorReply = %q, %q; want %q, %q", typ, msg, tt.typ, tt.msg)
			}
		})
	}
}

func TestScrubDetailCutsEchoesOfTheRequest(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"prompt is too long: 215000 tokens > 200000 maximum", "prompt is too long: 215000 tokens > 200000 maximum"},
		{`messages.0.content.0.text: invalid text "Weekend feast: free dessert inside"`, "messages.0.content.0.text: invalid text …"},
		{"tools.0.input_schema: `type` must be a string", "tools.0.input_schema: … must be a string"},
		{"field 'intent' doesn't match", "field … doesn't match"},
		{"two\nlines\t and  spaces", "two lines and spaces"},
		{"bad key sk-test-SECRET", "bad key …"},
		{"Incorrect API key provided: sk-proj-****abcd. You can find your key", "Incorrect API key provided: … You can find your key"},
		{"invalid header Bearer abc.def.ghi", "invalid header …"},
		{"token AbCdEf0123456789XyZ0123456789 rejected", "token … rejected"},
		{"messages.0.content.0.text: field required", "messages.0.content.0.text: field required"},
		{"invalid_request_error and authentication_error stay", "invalid_request_error and authentication_error stay"},
		{strings.Repeat("word ", 80), strings.Repeat("word ", 60) + "…"},
	} {
		if got := scrubDetail(tt.in); got != tt.want {
			t.Errorf("scrubDetail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Through each kind of adapter: the SDK one (Anthropic) and the plain HTTP one (Ollama).
func TestRefusalReasonReachesTheError(t *testing.T) {
	const quotedSubject, address = "Weekend feast: free dessert inside", "asha.rao@gmail.com"
	for _, tt := range []struct {
		name   string
		reply  string
		header map[string]string
		gen    func(url string) Generator
		want   []string
	}{
		{
			name:   "anthropic",
			reply:  `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 215000 tokens > 200000 maximum, at \"` + quotedSubject + `\" from ` + address + `"}}`,
			header: map[string]string{"request-id": "req_abc"},
			gen: func(url string) Generator {
				d := testDeps()
				d.AnthropicURL = url
				return NewAnthropic(AnthropicAuth{APIKey: "sk-test"}, "claude-haiku-4-5", d)
			},
			want: []string{"anthropic: HTTP 400 Bad Request", "invalid_request_error", "prompt is too long: 215000 tokens > 200000 maximum", "(request req_abc)"},
		},
		{
			name:  "ollama",
			reply: `{"error":"model \"llama9\" not found, try pulling it first"}`,
			gen:   func(url string) Generator { return NewOllama(url, "llama9", testDeps()) },
			want:  []string{"ollama: HTTP 400 Bad Request", "model … not found, try pulling it first"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tt.header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(tt.reply))
			}))
			defer srv.Close()

			var out map[string]any
			_, err := tt.gen(srv.URL).Generate(t.Context(), "system", "user", json.RawMessage(`{"type":"object","properties":{}}`), &out)
			var status *StatusError
			if !errors.As(err, &status) || status.Code != http.StatusBadRequest {
				t.Fatalf("err = %v, want a StatusError 400", err)
			}
			msg := err.Error()
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error %q lacks %q", msg, w)
				}
			}
			for _, leak := range []string{quotedSubject, address, "llama9", "sk-test"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error %q carries %q from the request", msg, leak)
				}
			}
		})
	}
}
