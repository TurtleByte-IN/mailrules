package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/models/anthropictest"
)

// fakeAnthropic serves an anthropictest fake and returns deps that send Claude requests to it.
func fakeAnthropic(t *testing.T, cfg anthropictest.Config) (*anthropictest.Server, Deps) {
	t.Helper()
	fake := anthropictest.New(cfg)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	d := testDeps()
	d.AnthropicURL = srv.URL
	return fake, d
}

func TestIsWorkspaceNeeded(t *testing.T) {
	refusal := func(provider string, code int, typ, detail string) error {
		return fmt.Errorf("primary decider anthropic: %w", &StatusError{Provider: provider, Code: code, Type: typ, Detail: scrubDetail(detail)})
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"Anthropic's refusal", refusal("anthropic", 400, "invalid_request_error", anthropictest.WorkspaceNeeded), true},
		{"worded otherwise but naming the header", refusal("anthropic", 400, "invalid_request_error", "Missing anthropic-workspace-id header."), true},
		{"another invalid request", refusal("anthropic", 400, "invalid_request_error", "prompt is too long: 215000 tokens > 200000 maximum"), false},
		{"the same words from another provider", refusal("openai", 400, "invalid_request_error", anthropictest.WorkspaceNeeded), false},
		{"another status", refusal("anthropic", 403, "permission_error", anthropictest.WorkspaceNeeded), false},
		{"another type", refusal("anthropic", 400, "api_error", anthropictest.WorkspaceNeeded), false},
		{"not a status error", errors.New(anthropictest.WorkspaceNeeded), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsWorkspaceNeeded(tt.err); got != tt.want {
				t.Errorf("IsWorkspaceNeeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// Every Claude adapter built from a configuration names its workspace on every request, and
// none when there is none: the composer, Claude as the decision model, and the fallback.
func TestEveryClaudeAdapterNamesTheWorkspace(t *testing.T) {
	answer := `{"rule_id":12,"confidence":0.93,"reason":"Order update."}`
	adapters := []struct {
		name string
		call func(t *testing.T, cfg *config.Config, d Deps) error
	}{
		{"composer", func(t *testing.T, cfg *config.Config, d Deps) error {
			g, err := NewGenerator(cfg, "claude-haiku-4-5", d)
			if err != nil {
				t.Fatal(err)
			}
			var out map[string]any
			_, err = g.Generate(t.Context(), "system", "user", json.RawMessage(`{"type":"object"}`), &out)
			return err
		}},
		{"decider", func(t *testing.T, cfg *config.Config, d Deps) error {
			r, err := NewRouter(cfg, "anthropic", d, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = r.Primary.Decide(t.Context(), testRequest)
			return err
		}},
		{"fallback", func(t *testing.T, cfg *config.Config, d Deps) error {
			r, err := NewRouter(cfg, "jev", d, nil)
			if err != nil || r.Fallback == nil {
				t.Fatalf("router %v, %v: want a fallback", r, err)
			}
			_, _, err = r.Fallback.Decide(t.Context(), testRequest)
			return err
		}},
	}
	for _, a := range adapters {
		for _, ws := range []string{"", "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ"} {
			t.Run(a.name+" workspace "+ws, func(t *testing.T) {
				fake, d := fakeAnthropic(t, anthropictest.Config{Answer: answer})
				cfg := &config.Config{AnthropicAPIKey: secret, AnthropicWorkspaceID: ws, OpenRouterAPIKey: secret, FallbackModel: DefaultAnthropicModel}
				if err := a.call(t, cfg, d); err != nil {
					t.Fatal(err)
				}
				if got := fake.Seen().MessageWorkspaces; !slices.Equal(got, []string{ws}) {
					t.Errorf("workspaces sent = %q, want [%q]", got, ws)
				}
			})
		}
	}
}

// A request refused for want of a workspace asks Deps.WorkspaceNeeded once and, given one,
// is made again in it; given none, the refusal stands.
func TestAnthropicRetriesInTheWorkspaceFound(t *testing.T) {
	tests := []struct {
		name      string
		workspace string // the adapter's
		found     string // what WorkspaceNeeded answers
		asked     int
		sent      []string
		wantErr   bool
	}{
		{"found: made again in it", "", "wrkspc_found", 1, []string{"", "wrkspc_found"}, false},
		{"none found: refused", "", "", 1, []string{""}, true},
		{"a workspace named: never asked", "wrkspc_set", "wrkspc_found", 0, []string{"wrkspc_set"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, d := fakeAnthropic(t, anthropictest.Config{NeedsWorkspace: true})
			asked := 0
			d.WorkspaceNeeded = func(_ context.Context, apiKey string) string {
				asked++
				if apiKey != secret {
					t.Errorf("asked about another key")
				}
				return tt.found
			}
			var out map[string]any
			_, err := NewAnthropic(AnthropicAuth{APIKey: secret, WorkspaceID: tt.workspace}, DefaultAnthropicModel, d).
				Generate(t.Context(), "s", "u", json.RawMessage(`{"type":"object"}`), &out)
			if (err != nil) != tt.wantErr || (err != nil && !IsWorkspaceNeeded(err)) {
				t.Errorf("err = %v", err)
			}
			if asked != tt.asked || !slices.Equal(fake.Seen().MessageWorkspaces, tt.sent) {
				t.Errorf("asked %d times, sent %q; want %d, %q", asked, fake.Seen().MessageWorkspaces, tt.asked, tt.sent)
			}
		})
	}
}

func TestAnthropicWorkspacesFind(t *testing.T) {
	one := []anthropictest.Workspace{{ID: "wrkspc_default", Name: "Default"}}
	two := append(one, anthropictest.Workspace{ID: "wrkspc_mail", Name: "Mail"})
	tests := []struct {
		name    string
		cfg     anthropictest.Config
		want    WorkspaceAnswer
		wantErr string
		probes  int
	}{
		{"one", anthropictest.Config{Workspaces: one}, WorkspaceAnswer{Workspaces: []Workspace{{"wrkspc_default", "Default"}}}, "", 0},
		{"several", anthropictest.Config{Workspaces: two}, WorkspaceAnswer{Workspaces: []Workspace{{"wrkspc_default", "Default"}, {"wrkspc_mail", "Mail"}}}, "", 0},
		{"a key made for one workspace", anthropictest.Config{ListStatus: http.StatusForbidden}, WorkspaceAnswer{NoneNeeded: true}, "", 1},
		{"a key that may not list", anthropictest.Config{ListStatus: http.StatusForbidden, ProbeStatus: http.StatusBadRequest}, WorkspaceAnswer{}, "permission_error", 1},
		{"a bad key", anthropictest.Config{ListStatus: http.StatusUnauthorized, ProbeStatus: http.StatusUnauthorized}, WorkspaceAnswer{}, "authentication_error", 1},
		{"no workspace listed", anthropictest.Config{}, WorkspaceAnswer{}, "no workspace", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake, d := fakeAnthropic(t, tt.cfg)
			got, err := AnthropicWorkspaces{Deps: d}.Find(t.Context(), secret)
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if err == nil && (got.NoneNeeded != tt.want.NoneNeeded || !slices.Equal(got.Workspaces, tt.want.Workspaces)) {
				t.Errorf("answer = %+v, want %+v", got, tt.want)
			}
			seen := fake.Seen()
			if seen.Lists != 1 || seen.Probes != tt.probes || !strings.Contains(seen.ListQuery, "include_default=true") {
				t.Errorf("lists %d (query %q), probes %d; want 1 with include_default=true, %d", seen.Lists, seen.ListQuery, seen.Probes, tt.probes)
			}
			if err != nil && strings.Contains(err.Error(), secret) {
				t.Errorf("the error holds the key: %v", err)
			}
			for _, k := range seen.APIKeys {
				if k != secret {
					t.Errorf("sent with key %q", k)
				}
			}
		})
	}
}
