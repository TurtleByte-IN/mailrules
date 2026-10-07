package settings

// The Claude workspace (MAI-57). An Anthropic key made for every workspace its owner can
// use must name one on each request; anthropic_workspace_id is that workspace. When it is
// empty and a Claude key is in force, MailRules asks Anthropic itself (models.AnthropicWorkspaces):
// when the key is saved or replaced, when Settings asks (LookupWorkspaces), and once when a
// Claude request is refused for want of it. What it learnt is kept in the settings table, in
// lookupRow, tied to the key by a fingerprint, never the key.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/TurtleByte-IN/mailrules/internal/config"
	"github.com/TurtleByte-IN/mailrules/internal/models"
)

const (
	// settingWorkspace is the setting, as the API spells it.
	settingWorkspace = "anthropic_workspace_id"
	// lookupRow holds what the last lookup learnt. It is not a setting: the API never
	// takes it, and only View shows what it says.
	lookupRow = "anthropic_workspace_lookup"
)

// What a lookup found out about the Claude key in force.
const (
	WorkspaceNoneNeeded = "none_needed" // the key was made for one workspace: nothing to name
	WorkspaceOne        = "one"         // one workspace, which MailRules stored as the setting
	WorkspaceSeveral    = "several"     // several: the user picks one
	WorkspaceFailed     = "failed"      // Anthropic could not say (no permission, network): the user types it
)

// WorkspaceFinder asks Anthropic which workspace a Claude key needs.
// models.AnthropicWorkspaces is the one the daemon uses.
type WorkspaceFinder interface {
	Find(ctx context.Context, apiKey string) (models.WorkspaceAnswer, error)
}

// WorkspaceLookup is what a lookup answers: its status and, for one or several, the
// organisation's workspaces (never nil).
type WorkspaceLookup struct {
	Status     string
	Workspaces []models.Workspace
}

// workspaceLookup is lookupRow as stored.
type workspaceLookup struct {
	Key        string             `json:"key"` // the fingerprint of the key it is about
	Status     string             `json:"status"`
	Workspaces []models.Workspace `json:"workspaces,omitempty"`
	Found      string             `json:"found,omitempty"`   // the ID MailRules stored as the setting itself
	Refused    bool               `json:"refused,omitempty"` // a Claude request was refused for want of a workspace
}

func (l workspaceLookup) answer() WorkspaceLookup {
	ws := l.Workspaces
	if ws == nil {
		ws = []models.Workspace{}
	}
	return WorkspaceLookup{Status: l.Status, Workspaces: ws}
}

// fingerprint ties lookupRow to one key without storing it: an HMAC under the master key.
// It is "" for no key.
func (s *Settings) fingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	m := hmac.New(sha256.New, s.Master)
	_, _ = m.Write([]byte("anthropic_api_key\x00" + apiKey))
	return hex.EncodeToString(m.Sum(nil)[:16])
}

// readLookup decodes lookupRow; a missing or unreadable row is no lookup.
func readLookup(rows map[string]string) workspaceLookup {
	var l workspaceLookup
	if v, ok := rows[lookupRow]; ok && json.Unmarshal([]byte(v), &l) != nil {
		return workspaceLookup{}
	}
	return l
}

// lookupFor is lookupRow when it is about apiKey, and no lookup otherwise.
func (s *Settings) lookupFor(rows map[string]string, apiKey string) workspaceLookup {
	if l := readLookup(rows); l.Key != "" && l.Key == s.fingerprint(apiKey) {
		return l
	}
	return workspaceLookup{}
}

// workspaceWarning is the warning for a Claude key that Anthropic refused for want of a
// workspace, while no lookup could settle which (several to choose from, or none listed).
func workspaceWarning(cfg config.Config, l workspaceLookup) []Warning {
	if cfg.AnthropicAPIKey == "" || cfg.AnthropicWorkspaceID != "" || !l.Refused || (l.Status != WorkspaceSeveral && l.Status != WorkspaceFailed) {
		return nil
	}
	return []Warning{{Code: "anthropic_workspace_needed", Path: settingWorkspace,
		Message: "Your Claude key covers your whole organisation, so Claude refuses MailRules' requests until a workspace is chosen. Choose the Anthropic workspace under the key."}}
}

// LookupWorkspaces says which workspace the Claude key in force needs, as the Settings
// screen asks. A known answer (none needed, one, several) is given again without asking
// Anthropic; otherwise Anthropic is asked, and one workspace found while none is set is
// stored as the setting.
func (s *Settings) LookupWorkspaces(ctx context.Context) (WorkspaceLookup, error) {
	s.lookupMu.Lock()
	defer s.lookupMu.Unlock()
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		return WorkspaceLookup{}, err
	}
	cfg, _, err := s.effective(rows)
	if err != nil {
		return WorkspaceLookup{}, err
	}
	if cfg.AnthropicAPIKey == "" {
		return workspaceLookup{Status: WorkspaceNoneNeeded}.answer(), nil // no key, nothing to name
	}
	l := s.lookupFor(rows, cfg.AnthropicAPIKey)
	if l.Status == WorkspaceNoneNeeded || l.Status == WorkspaceOne || l.Status == WorkspaceSeveral {
		return l.answer(), nil
	}
	return s.lookup(ctx, cfg, l, false)
}

// applyWorkspace adds to set a change of anthropic_workspace_id, and what a saved, replaced
// or removed Claude key undoes: what was learnt about the old key, and a workspace MailRules
// found for it unless the patch names one. storedWorkspace is the setting's row before.
func (s *Settings) applyWorkspace(p Patch, cfg *config.Config, before workspaceLookup, storedWorkspace string, set map[string]*string) error {
	named := p.AnthropicWorkspaceID != nil || slices.Contains(p.Reset, settingWorkspace)
	if p.AnthropicWorkspaceID != nil {
		id := strings.TrimSpace(*p.AnthropicWorkspaceID)
		if !config.ValidWorkspaceID(id) {
			return &Invalid{settingWorkspace, "A workspace ID starts with wrkspc_ followed by letters and digits. Copy it from the ID column of Settings → Workspaces in the Claude Console."}
		}
		b, _ := json.Marshal(id)
		v := string(b)
		set[settingWorkspace], cfg.AnthropicWorkspaceID = &v, id
	}
	var stored string
	_ = json.Unmarshal([]byte(storedWorkspace), &stored) // no row: ""
	_, keyChanged := p.Keys["anthropic_api_key"]
	switch {
	case keyChanged:
		set[lookupRow] = nil
		if !named && before.Found != "" && stored == before.Found {
			set[settingWorkspace], cfg.AnthropicWorkspaceID = nil, s.Env.AnthropicWorkspaceID
		}
	case named && before.Key != "" && cfg.AnthropicWorkspaceID == "":
		set[lookupRow] = nil // the next lookup starts afresh
	case named && before.Found != "":
		before.Found = "" // the workspace is now the user's choice
		b, err := json.Marshal(before)
		if err != nil {
			return fmt.Errorf("encode the workspace lookup: %w", err)
		}
		v := string(b)
		set[lookupRow] = &v
	}
	return nil
}

// lookup asks Anthropic and stores what it said. The caller holds lookupMu; l is what was
// known about this key before. refused records that a Claude request was refused.
func (s *Settings) lookup(ctx context.Context, cfg config.Config, l workspaceLookup, refused bool) (WorkspaceLookup, error) {
	next := workspaceLookup{Key: s.fingerprint(cfg.AnthropicAPIKey), Refused: refused || l.Refused, Found: l.Found}
	if s.Workspaces == nil {
		next.Status = WorkspaceFailed
	} else {
		ans, err := s.Workspaces.Find(ctx, cfg.AnthropicAPIKey)
		switch {
		case err != nil && ctx.Err() != nil:
			return WorkspaceLookup{}, fmt.Errorf("look up the Anthropic workspaces: %w", ctx.Err())
		case err != nil:
			// The error is a scrubbed StatusError or a network error: never the key.
			slog.WarnContext(ctx, "could not look up the Anthropic workspaces; the workspace has to be chosen in Settings", "error", err.Error())
			next.Status = WorkspaceFailed
		case ans.NoneNeeded:
			next.Status = WorkspaceNoneNeeded
		case len(ans.Workspaces) == 1:
			next.Status, next.Workspaces = WorkspaceOne, ans.Workspaces
		default:
			next.Status, next.Workspaces = WorkspaceSeveral, ans.Workspaces
		}
	}
	set := map[string]*string{}
	if next.Status == WorkspaceOne && cfg.AnthropicWorkspaceID == "" {
		id := next.Workspaces[0].ID
		b, _ := json.Marshal(id)
		v := string(b)
		set[settingWorkspace], next.Found = &v, id
	}
	b, err := json.Marshal(next)
	if err != nil {
		return WorkspaceLookup{}, fmt.Errorf("encode the workspace lookup: %w", err)
	}
	v := string(b)
	set[lookupRow] = &v
	if err := s.Store.SetSettings(ctx, set); err != nil {
		return WorkspaceLookup{}, err
	}
	slog.InfoContext(ctx, "looked up the Anthropic workspace", "status", next.Status, "workspaces", len(next.Workspaces), "stored", set[settingWorkspace] != nil)
	return next.answer(), nil
}

// workspaceNeeded is models.Deps.WorkspaceNeeded: a Claude request made with apiKey was
// refused for want of a workspace. It answers the workspace to retry in: the setting, when
// it was set since the request's adapter was built, or the one workspace a lookup finds.
// The lookup runs once per key: after it found several or failed, later refusals are only
// recorded (Settings shows the warning), and asking again is Settings' business.
func (s *Settings) workspaceNeeded(ctx context.Context, apiKey string) string {
	s.lookupMu.Lock()
	defer s.lookupMu.Unlock()
	rows, err := s.Store.Settings(ctx)
	if err != nil {
		slog.WarnContext(ctx, "could not read settings to find the Anthropic workspace", "error", err.Error())
		return ""
	}
	cfg, _, err := s.effective(rows)
	switch {
	case err != nil:
		slog.WarnContext(ctx, "stored settings cannot be used to find the Anthropic workspace", "error", err.Error())
		return ""
	case cfg.AnthropicAPIKey != apiKey: // the key changed since; the next request uses the new one
		return ""
	case cfg.AnthropicWorkspaceID != "":
		return cfg.AnthropicWorkspaceID
	}
	l := s.lookupFor(rows, apiKey)
	if l.Refused && (l.Status == WorkspaceSeveral || l.Status == WorkspaceFailed) {
		return ""
	}
	if l.Status == WorkspaceSeveral { // already listed: only the refusal is new
		l.Refused = true
		b, _ := json.Marshal(l)
		v := string(b)
		if err := s.Store.SetSettings(ctx, map[string]*string{lookupRow: &v}); err != nil {
			slog.WarnContext(ctx, "could not record the refused Claude request", "error", err.Error())
		}
		return ""
	}
	res, err := s.lookup(ctx, cfg, l, true)
	if err != nil {
		slog.WarnContext(ctx, "could not look up the Anthropic workspace", "error", err.Error())
		return ""
	}
	if res.Status == WorkspaceOne {
		return res.Workspaces[0].ID
	}
	return ""
}

// deps are the adapters' Deps, with the refusal of a Claude request for want of a
// workspace sent to workspaceNeeded.
func (s *Settings) deps() models.Deps {
	d := s.Deps
	d.WorkspaceNeeded = s.workspaceNeeded
	return d
}
