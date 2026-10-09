package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/TurtleByte-IN/mailrules/internal/models"
	"github.com/TurtleByte-IN/mailrules/internal/models/anthropictest"
)

const claudeKey = "sk-ant-api03-ORGANISATION-KEY"

var (
	oneWorkspace  = []anthropictest.Workspace{{ID: "wrkspc_default", Name: "Default"}}
	twoWorkspaces = []anthropictest.Workspace{{ID: "wrkspc_default", Name: "Default"}, {ID: "wrkspc_mail", Name: "Mail"}}
	// Anthropic's answers for each kind of key, as anthropictest plays them.
	orgKeyOne     = anthropictest.Config{Workspaces: oneWorkspace, NeedsWorkspace: true}
	orgKeySeveral = anthropictest.Config{Workspaces: twoWorkspaces, NeedsWorkspace: true}
	scopedKey     = anthropictest.Config{ListStatus: http.StatusForbidden}
	orgKeyNoList  = anthropictest.Config{ListStatus: http.StatusForbidden, ProbeStatus: http.StatusBadRequest, NeedsWorkspace: true}
)

// withAnthropic is newSettings whose Claude requests and workspace lookups go to a fake.
func withAnthropic(t *testing.T, env map[string]string, cfg anthropictest.Config) (*Settings, *anthropictest.Server) {
	t.Helper()
	s := newSettings(t, env)
	fake := anthropictest.New(cfg)
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	s.Deps.AnthropicURL = srv.URL
	s.Workspaces = models.AnthropicWorkspaces{Deps: s.Deps}
	return s, fake
}

func compose(t *testing.T, s *Settings) error {
	t.Helper()
	gen, err := s.Composer(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_, err = gen.Generate(t.Context(), "system", "user", json.RawMessage(`{"type":"object"}`), &out)
	return err
}

func view(t *testing.T, s *Settings) View {
	t.Helper()
	v, err := s.View(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// workspaceWarned reports whether Settings warn that a workspace is needed, pointing at the field.
func workspaceWarned(v View) bool {
	return slices.ContainsFunc(v.Warnings, func(w Warning) bool {
		return w.Code == "anthropic_workspace_needed" && w.Path == "anthropic_workspace_id" && strings.Contains(w.Message, "workspace")
	})
}

// anthropic_workspace_id is empty or a wrkspc_ ID; anything else is refused on its field.
func TestWorkspaceIDIsCheckedOnSave(t *testing.T) {
	tests := []struct {
		in, want string // want: what is stored, when it is not refused
		refused  bool
	}{
		{"wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ", "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ", false},
		{"  wrkspc_01Jw  ", "wrkspc_01Jw", false},
		{"", "", false},
		{"default", "", true},
		{"wrkspc_", "", true},
		{"wrkspc_01Jw\r\nX-Other: 1", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			s := newSettings(t, nil)
			err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new(tt.in)})
			var bad *Invalid
			if tt.refused {
				if !errors.As(err, &bad) || bad.Path != "anthropic_workspace_id" || !strings.Contains(bad.Message, "wrkspc_") {
					t.Fatalf("err = %v, want a refusal on anthropic_workspace_id", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := view(t, s).AnthropicWorkspaceID; got != tt.want {
				t.Errorf("stored %q, want %q", got, tt.want)
			}
		})
	}
	// null forgets the stored one: the environment's is back.
	s := newSettings(t, map[string]string{"ANTHROPIC_WORKSPACE_ID": "wrkspc_env"})
	if err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new("wrkspc_mine")}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(t.Context(), 1, Patch{Reset: []string{"anthropic_workspace_id"}}); err != nil {
		t.Fatal(err)
	}
	if got := view(t, s).AnthropicWorkspaceID; got != "wrkspc_env" {
		t.Errorf("after reset %q, want the environment's", got)
	}
}

// Every Claude request the daemon makes names the workspace in force, and none when there is
// none: the composer, Claude as the decision model, and the fallback.
func TestEveryClaudeRequestNamesTheWorkspace(t *testing.T) {
	answer := `{"rule_id":12,"confidence":0.93,"reason":"Order update."}`
	req := models.DecideRequest{Candidates: []models.Candidate{{RuleID: 12, Name: "Food", Intent: "Food orders"}}}
	paths := []struct {
		name string
		env  map[string]string
		call func(t *testing.T, s *Settings) error
	}{
		{"composer", nil, compose},
		{"decider", map[string]string{"MAILRULES_DECIDER": "anthropic"}, func(t *testing.T, s *Settings) error {
			r, _ := s.Live(t.Context(), 1)
			_, _, err := r.Primary.Decide(t.Context(), req)
			return err
		}},
		{"fallback", map[string]string{"OPENROUTER_API_KEY": "sk-or-test"}, func(t *testing.T, s *Settings) error {
			r, _ := s.Live(t.Context(), 1)
			if r == nil || r.Fallback == nil {
				t.Fatal("no fallback")
			}
			_, _, err := r.Fallback.Decide(t.Context(), req)
			return err
		}},
	}
	for _, p := range paths {
		for _, ws := range []string{"", "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ"} {
			t.Run(p.name+" "+ws, func(t *testing.T) {
				env := map[string]string{"ANTHROPIC_API_KEY": claudeKey}
				for k, v := range p.env {
					env[k] = v
				}
				s, fake := withAnthropic(t, env, anthropictest.Config{Answer: answer})
				if err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new(ws)}); err != nil {
					t.Fatal(err)
				}
				if err := p.call(t, s); err != nil {
					t.Fatal(err)
				}
				if got := fake.Seen().MessageWorkspaces; !slices.Equal(got, []string{ws}) {
					t.Errorf("workspaces sent = %q, want [%q]", got, ws)
				}
			})
		}
	}
}

// Saving a Claude key looks its workspace up: one is stored, several are listed, a key made
// for one workspace needs none, and a key that cannot list is left to the user. What is
// known is given again without asking Anthropic; a failure is asked again.
func TestSavingAKeyLooksUpItsWorkspace(t *testing.T) {
	tests := []struct {
		name       string
		anthropic  anthropictest.Config
		status     string
		workspaces int
		stored     string
		lists      int // after a second LookupWorkspaces
		sent       string
	}{
		{"one", orgKeyOne, WorkspaceOne, 1, "wrkspc_default", 1, "wrkspc_default"},
		{"several", orgKeySeveral, WorkspaceSeveral, 2, "", 1, ""},
		{"none needed", scopedKey, WorkspaceNoneNeeded, 0, "", 1, ""},
		{"failed", orgKeyNoList, WorkspaceFailed, 0, "", 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, fake := withAnthropic(t, nil, tt.anthropic)
			if err := s.Apply(t.Context(), 1, Patch{Keys: map[string]string{"anthropic_api_key": claudeKey}}); err != nil {
				t.Fatal(err)
			}
			v := view(t, s)
			if v.AnthropicWorkspaceID != tt.stored || v.AnthropicWorkspaceFound != (tt.stored != "") || (tt.stored != "" && v.AnthropicWorkspaceName != "Default") {
				t.Errorf("view = %q %q found %v, want %q found", v.AnthropicWorkspaceID, v.AnthropicWorkspaceName, v.AnthropicWorkspaceFound, tt.stored)
			}
			if workspaceWarned(v) {
				t.Errorf("warned before any refusal: %v", v.Warnings)
			}
			got, err := s.LookupWorkspaces(t.Context(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != tt.status || len(got.Workspaces) != tt.workspaces {
				t.Errorf("lookup = %+v, want %s with %d", got, tt.status, tt.workspaces)
			}
			if fake.Seen().Lists != tt.lists {
				t.Errorf("List Workspaces called %d times, want %d", fake.Seen().Lists, tt.lists)
			}
			if tt.status == WorkspaceNoneNeeded {
				_ = compose(t, s)
				if sent := fake.Seen().MessageWorkspaces; !slices.Equal(sent, []string{tt.sent}) {
					t.Errorf("sent %q, want [%q]", sent, tt.sent)
				}
			}
		})
	}
}

// Replacing or removing the key forgets a workspace MailRules found for it, and what it
// learnt; a workspace the user chose stays.
func TestReplacingTheKeyForgetsTheWorkspaceFound(t *testing.T) {
	s, fake := withAnthropic(t, nil, orgKeyOne)
	saveKey := func(k string) {
		t.Helper()
		if err := s.Apply(t.Context(), 1, Patch{Keys: map[string]string{"anthropic_api_key": k}}); err != nil {
			t.Fatal(err)
		}
	}
	saveKey(claudeKey)
	if v := view(t, s); v.AnthropicWorkspaceID != "wrkspc_default" || !v.AnthropicWorkspaceFound {
		t.Fatalf("found = %+v", v)
	}

	fake.Set(orgKeySeveral)
	saveKey(claudeKey + "-NEW")
	if v := view(t, s); v.AnthropicWorkspaceID != "" || v.AnthropicWorkspaceFound {
		t.Errorf("after replacing the key, the old key's workspace is still in force: %+v", v)
	}
	if got, _ := s.LookupWorkspaces(t.Context(), 1); got.Status != WorkspaceSeveral {
		t.Errorf("lookup = %+v", got)
	}

	// The user picks one: it is theirs, so a new key keeps it, and it is not looked up.
	if err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new("wrkspc_mail")}); err != nil {
		t.Fatal(err)
	}
	if v := view(t, s); v.AnthropicWorkspaceID != "wrkspc_mail" || v.AnthropicWorkspaceName != "Mail" || v.AnthropicWorkspaceFound {
		t.Errorf("chosen = %+v", v)
	}
	lists := fake.Seen().Lists
	saveKey(claudeKey + "-THIRD")
	if v := view(t, s); v.AnthropicWorkspaceID != "wrkspc_mail" {
		t.Errorf("a new key dropped the workspace the user chose: %+v", v)
	}
	if fake.Seen().Lists != lists {
		t.Errorf("looked up although a workspace is set")
	}

	// Removing the key forgets a found workspace too.
	fake.Set(orgKeyOne)
	if err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new("")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LookupWorkspaces(t.Context(), 1); got.Status != WorkspaceOne {
		t.Fatalf("lookup = %+v", got)
	}
	if err := s.Apply(t.Context(), 1, Patch{Keys: map[string]string{"anthropic_api_key": ""}}); err != nil {
		t.Fatal(err)
	}
	if v := view(t, s); v.AnthropicWorkspaceID != "" {
		t.Errorf("after removing the key: %+v", v)
	}
}

// A Claude request refused for want of a workspace, with none set, looks it up once: with one
// workspace it is stored and the request made again in it; otherwise the refusal stands,
// Settings warns, and later refusals do not ask Anthropic again.
func TestARefusedRequestLooksUpTheWorkspaceOnce(t *testing.T) {
	tests := []struct {
		name    string
		cfg     anthropictest.Config
		ok      bool
		sent    []string // after two compositions
		warning bool
	}{
		{"one", orgKeyOne, true, []string{"", "wrkspc_default", "wrkspc_default"}, false},
		{"several", orgKeySeveral, false, []string{"", ""}, true},
		{"failed", orgKeyNoList, false, []string{"", ""}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(old) })

			// A key from the environment: saved before this release, never looked up.
			s, fake := withAnthropic(t, map[string]string{"ANTHROPIC_API_KEY": claudeKey}, tt.cfg)
			for range 2 {
				if err := compose(t, s); (err == nil) != tt.ok || (err != nil && !models.IsWorkspaceNeeded(err)) {
					t.Fatalf("compose = %v", err)
				}
			}
			if sent := fake.Seen().MessageWorkspaces; !slices.Equal(sent, tt.sent) {
				t.Errorf("sent %q, want %q", sent, tt.sent)
			}
			if fake.Seen().Lists != 1 {
				t.Errorf("List Workspaces called %d times, want once", fake.Seen().Lists)
			}
			v := view(t, s)
			if workspaceWarned(v) != tt.warning {
				t.Errorf("warnings = %v, want the workspace warning: %v", v.Warnings, tt.warning)
			}
			if tt.ok && (v.AnthropicWorkspaceID != "wrkspc_default" || !v.AnthropicWorkspaceFound) {
				t.Errorf("view = %+v", v)
			}
			// Choosing one clears the warning.
			if err := s.Apply(t.Context(), 1, Patch{AnthropicWorkspaceID: new("wrkspc_default")}); err != nil {
				t.Fatal(err)
			}
			if v := view(t, s); workspaceWarned(v) {
				t.Errorf("warnings once a workspace is set: %v", v.Warnings)
			}
			if strings.Contains(logs.String(), claudeKey) {
				t.Errorf("the log holds the key:\n%s", logs.String())
			}
		})
	}
}
