// Package anthropictest is a fake Anthropic API for tests: the Messages API, token counting
// and List Workspaces, answering as the test sets it up and recording what it was sent.
// Serve it with httptest.NewServer and point models.Deps.AnthropicURL at it.
package anthropictest

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// WorkspaceNeeded is Anthropic's refusal of a request that names no workspace, made with a
// key that covers a whole organisation (as the daemon logged it on 2026-10-07, MAI-57).
const WorkspaceNeeded = "This API key is not scoped to a workspace, so this request must include the anthropic-workspace-id header " +
	"with the ID of the workspace to use. Add the header, or use an API key that is scoped to a workspace."

// Workspace is one workspace List Workspaces answers.
type Workspace struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Config is how the fake answers.
type Config struct {
	Workspaces     []Workspace // what List Workspaces lists
	ListStatus     int         // non-zero: List Workspaces is refused with this status
	ProbeStatus    int         // non-zero: count_tokens is refused with this status; 400 = for want of a workspace
	NeedsWorkspace bool        // a message that names no workspace is refused with WorkspaceNeeded
	Answer         string      // the tool input every message answers with; empty = {}
}

// Seen is what the fake was sent.
type Seen struct {
	Lists, Probes     int
	ListQuery         string   // the query of the last List Workspaces call
	MessageWorkspaces []string // the anthropic-workspace-id of every message, in order; "" = none
	APIKeys           []string // the x-api-key of every request
}

// Server is the fake, an http.Handler.
type Server struct {
	mu   sync.Mutex
	cfg  Config
	seen Seen
}

// New returns a fake that answers as cfg says.
func New(cfg Config) *Server { return &Server{cfg: cfg} }

// Set changes how the fake answers from the next request on.
func (s *Server) Set(cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg = cfg
}

// Seen returns what the fake was sent so far.
func (s *Server) Seen() Seen {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.seen
	out.MessageWorkspaces = append([]string(nil), s.seen.MessageWorkspaces...)
	out.APIKeys = append([]string(nil), s.seen.APIKeys...)
	return out
}

func refuse(w http.ResponseWriter, status int, typ, message string) {
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": typ, "message": message}, "request_id": "req_fake"})
	_, _ = w.Write(b)
}

// refuseAs refuses with the error type Anthropic gives for status.
func refuseAs(w http.ResponseWriter, status int) {
	switch status {
	case http.StatusBadRequest:
		refuse(w, status, "invalid_request_error", WorkspaceNeeded)
	case http.StatusUnauthorized:
		refuse(w, status, "authentication_error", "invalid x-api-key")
	case http.StatusForbidden:
		refuse(w, status, "permission_error", "This API key does not have permission to use the Admin API.")
	default:
		refuse(w, status, "api_error", "Internal server error")
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_fake")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen.APIKeys = append(s.seen.APIKeys, r.Header.Get("X-Api-Key"))
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/organizations/workspaces":
		s.seen.Lists++
		s.seen.ListQuery = r.URL.RawQuery
		if s.cfg.ListStatus != 0 {
			refuseAs(w, s.cfg.ListStatus)
			return
		}
		data := make([]map[string]any, len(s.cfg.Workspaces))
		for i, ws := range s.cfg.Workspaces {
			data[i] = map[string]any{"type": "workspace", "id": ws.ID, "name": ws.Name, "archived_at": nil,
				"created_at": "2026-01-01T00:00:00Z", "display_color": "#6C5BB9"}
		}
		b, _ := json.Marshal(map[string]any{"data": data, "has_more": false, "first_id": nil, "last_id": nil})
		_, _ = w.Write(b)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages/count_tokens":
		s.seen.Probes++
		if s.cfg.ProbeStatus != 0 {
			refuseAs(w, s.cfg.ProbeStatus)
			return
		}
		_, _ = io.WriteString(w, `{"input_tokens":8}`)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		ws := r.Header.Get("Anthropic-Workspace-Id")
		s.seen.MessageWorkspaces = append(s.seen.MessageWorkspaces, ws)
		if s.cfg.NeedsWorkspace && ws == "" {
			refuseAs(w, http.StatusBadRequest)
			return
		}
		answer := s.cfg.Answer
		if answer == "" {
			answer = "{}"
		}
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-4-5","content":[{"type":"tool_use","id":"toolu_1","name":"answer","input":`+
			answer+`}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":100,"output_tokens":40}}`)
	default:
		refuse(w, http.StatusNotFound, "not_found_error", "Not found")
	}
}
